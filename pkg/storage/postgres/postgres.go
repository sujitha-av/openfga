// Package postgres contains an implementation of the storage interface that works with Postgres.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/cenkalti/backoff/v4"
	_ "github.com/jackc/pgx/v5/stdlib"
	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	"github.com/openfga/openfga/pkg/logger"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/storage/sqlcommon"
	tupleUtils "github.com/openfga/openfga/pkg/tuple"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var tracer = otel.Tracer("openfga/pkg/storage/postgres")

type Postgres struct {
	stbl                   sq.StatementBuilderType
	db                     *sql.DB
	logger                 logger.Logger
	maxTuplesPerWriteField int
	maxTypesPerModelField  int
}

var _ storage.OpenFGADatastore = (*Postgres)(nil)

func New(uri string, cfg *sqlcommon.Config) (*Postgres, error) {
	if cfg.Username != "" || cfg.Password != "" {
		parsed, err := url.Parse(uri)
		if err != nil {
			return nil, fmt.Errorf("failed to parse postgres connection uri: %w", err)
		}

		username := ""
		if cfg.Username != "" {
			username = cfg.Username
		} else if parsed.User != nil {
			username = parsed.User.Username()
		}

		if cfg.Password != "" {
			parsed.User = url.UserPassword(username, cfg.Password)
		} else if parsed.User != nil {
			if password, ok := parsed.User.Password(); ok {
				parsed.User = url.UserPassword(username, password)
			} else {
				parsed.User = url.User(username)
			}
		} else {
			parsed.User = url.User(username)
		}

		uri = parsed.String()
	}

	db, err := sql.Open("pgx", uri)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize postgres connection: %w", err)
	}

	if cfg.MaxOpenConns != 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}

	if cfg.MaxIdleConns != 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}

	if cfg.ConnMaxIdleTime != 0 {
		db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}

	if cfg.ConnMaxLifetime != 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}

	policy := backoff.NewExponentialBackOff()
	policy.MaxElapsedTime = 1 * time.Minute
	attempt := 1
	err = backoff.Retry(func() error {
		err = db.PingContext(context.Background())
		if err != nil {
			cfg.Logger.Info("waiting for postgres", zap.Int("attempt", attempt))
			attempt++
			return err
		}
		return nil
	}, policy)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize postgres connection: %w", err)
	}

	return &Postgres{
		stbl:                   sq.StatementBuilder.PlaceholderFormat(sq.Dollar).RunWith(db),
		db:                     db,
		logger:                 cfg.Logger,
		maxTuplesPerWriteField: cfg.MaxTuplesPerWriteField,
		maxTypesPerModelField:  cfg.MaxTypesPerModelField,
	}, nil
}

// Close closes any open connections and cleans up residual resources
// used by this storage adapter instance.
func (p *Postgres) Close() {
	p.db.Close()
}

func (p *Postgres) Read(ctx context.Context, store string, tupleKey *openfgav1.TupleKey) (storage.TupleIterator, error) {
	ctx, span := tracer.Start(ctx, "postgres.Read")
	defer span.End()

	return p.read(ctx, store, tupleKey, nil)
}

func (p *Postgres) ReadPage(ctx context.Context, store string, tupleKey *openfgav1.TupleKey, opts storage.PaginationOptions) ([]*openfgav1.Tuple, []byte, error) {
	ctx, span := tracer.Start(ctx, "postgres.ReadPage")
	defer span.End()

	iter, err := p.read(ctx, store, tupleKey, &opts)
	if err != nil {
		return nil, nil, err
	}
	defer iter.Stop()

	return iter.ToArray(opts)
}

func (p *Postgres) read(ctx context.Context, store string, tupleKey *openfgav1.TupleKey, opts *storage.PaginationOptions) (*sqlcommon.SQLTupleIterator, error) {
	ctx, span := tracer.Start(ctx, "postgres.read")
	defer span.End()

	sb := p.stbl.
		Select("store", "object_type", "object_id", "relation", "user_object_type", "user_object_id", "user_relation", "ulid", "inserted_at").
		From("tuple").
		Where(sq.Eq{"store": store})
	if opts != nil {
		sb = sb.OrderBy("ulid")
	}

	objectType, objectID := tupleUtils.SplitObject(tupleKey.GetObject())
	if objectType != "" {
		sb = sb.Where(sq.Eq{"object_type": objectType})
	}
	if objectID != "" {
		sb = sb.Where(sq.Eq{"object_id": objectID})
	}
	if tupleKey.GetRelation() != "" {
		sb = sb.Where(sq.Eq{"relation": tupleKey.GetRelation()})
	}
	if tupleKey.GetUser() != "" {
		userObjectType, userObjectID, userRelation := tupleUtils.ToUserParts(tupleKey.GetUser())
		sb = sb.Where(sq.Eq{"user_object_type": userObjectType})
		sb = sb.Where(sq.Eq{"user_object_id": userObjectID})
		sb = sb.Where(sq.Eq{"user_relation": userRelation})
	}
	if opts != nil && opts.From != "" {
		token, err := sqlcommon.UnmarshallContToken(opts.From)
		if err != nil {
			return nil, err
		}
		sb = sb.Where(sq.GtOrEq{"ulid": token.Ulid})
	}
	if opts != nil && opts.PageSize != 0 {
		sb = sb.Limit(uint64(opts.PageSize + 1)) // + 1 is used to determine whether to return a continuation token.
	}

	rows, err := sb.QueryContext(ctx)
	if err != nil {
		return nil, sqlcommon.HandleSQLError(err)
	}

	return sqlcommon.NewSQLTupleIterator(rows), nil
}

func (p *Postgres) Write(ctx context.Context, store string, deletes storage.Deletes, writes storage.Writes) error {
	ctx, span := tracer.Start(ctx, "postgres.Write")
	defer span.End()

	if len(deletes)+len(writes) > p.MaxTuplesPerWrite() {
		return storage.ErrExceededWriteBatchLimit
	}

	now := time.Now().UTC()
	return sqlcommon.Write(ctx, sqlcommon.NewDBInfo(p.db, p.stbl, "NOW()"), store, deletes, writes, now)
}

func (p *Postgres) ReadUserTuple(ctx context.Context, store string, tupleKey *openfgav1.TupleKey) (*openfgav1.Tuple, error) {
	ctx, span := tracer.Start(ctx, "postgres.ReadUserTuple")
	defer span.End()

	objectType, objectID := tupleUtils.SplitObject(tupleKey.GetObject())
	userObjectType, userObjectID, userRelation := tupleUtils.ToUserParts(tupleKey.GetUser())

	var record sqlcommon.TupleRecord
	err := p.stbl.
		Select("object_type", "object_id", "relation", "user_object_type", "user_object_id", "user_relation").
		From("tuple").
		Where(sq.Eq{
			"store":            store,
			"object_type":      objectType,
			"object_id":        objectID,
			"relation":         tupleKey.GetRelation(),
			"user_object_type": userObjectType,
			"user_object_id":   userObjectID,
			"user_relation":    userRelation,
		}).
		QueryRowContext(ctx).
		Scan(&record.ObjectType, &record.ObjectID, &record.Relation, &record.UserObjectType, &record.UserObjectID, &record.UserRelation)
	if err != nil {
		return nil, sqlcommon.HandleSQLError(err)
	}

	return record.AsTuple(), nil
}

func (p *Postgres) ReadUsersetTuples(ctx context.Context, store string, filter storage.ReadUsersetTuplesFilter) (storage.TupleIterator, error) {
	ctx, span := tracer.Start(ctx, "postgres.ReadUsersetTuples")
	defer span.End()

	sb := p.stbl.Select("store", "object_type", "object_id", "relation", "user_object_type", "user_object_id", "user_relation", "ulid", "inserted_at").
		From("tuple").
		Where(sq.Eq{"store": store})

	objectType, objectID := tupleUtils.SplitObject(filter.Object)
	if objectType != "" {
		sb = sb.Where(sq.Eq{"object_type": objectType})
	}
	if objectID != "" {
		sb = sb.Where(sq.Eq{"object_id": objectID})
	}
	if filter.Relation != "" {
		sb = sb.Where(sq.Eq{"relation": filter.Relation})
	}
	if len(filter.AllowedUserTypeRestrictions) > 0 {
		orConditions := sq.Or{}
		for _, userset := range filter.AllowedUserTypeRestrictions {
			if _, ok := userset.RelationOrWildcard.(*openfgav1.RelationReference_Relation); ok {
				orConditions = append(orConditions, sq.And{sq.Eq{"user_object_type": userset.GetType()}, sq.Eq{"user_relation": userset.GetRelation()}})
			}
			if _, ok := userset.RelationOrWildcard.(*openfgav1.RelationReference_Wildcard); ok {
				orConditions = append(orConditions, sq.And{sq.Eq{"user_object_type": userset.GetType()}, sq.Eq{"user_object_id": "*"}})
			}
		}
		sb = sb.Where(orConditions)
	}
	rows, err := sb.QueryContext(ctx)
	if err != nil {
		return nil, sqlcommon.HandleSQLError(err)
	}

	return sqlcommon.NewSQLTupleIterator(rows), nil
}

func (p *Postgres) ReadStartingWithUser(ctx context.Context, store string, opts storage.ReadStartingWithUserFilter) (storage.TupleIterator, error) {
	ctx, span := tracer.Start(ctx, "postgres.ReadStartingWithUser")
	defer span.End()

	iterators := make([]storage.TupleIterator, 0, len(opts.UserFilter))
	for _, u := range opts.UserFilter {
		userObjectType, userObjectID, userRelation := tupleUtils.ToUserPartsFromObjectRelation(u)

		rows, err := p.stbl.
			Select("store", "object_type", "object_id", "relation", "user_object_type", "user_object_id", "user_relation", "ulid", "inserted_at").
			From("tuple").
			Where(sq.Eq{
				"store":            store,
				"object_type":      opts.ObjectType,
				"relation":         opts.Relation,
				"user_object_type": userObjectType,
				"user_object_id":   userObjectID,
				"user_relation":    userRelation,
			}).QueryContext(ctx)
		if err != nil {
			return nil, sqlcommon.HandleSQLError(err)
		}

		iterators = append(iterators, sqlcommon.NewSQLTupleIterator(rows))
	}

	return storage.NewCombinedIterator(iterators...), nil
}

func (p *Postgres) MaxTuplesPerWrite() int {
	return p.maxTuplesPerWriteField
}

func (p *Postgres) ReadAuthorizationModel(ctx context.Context, store string, modelID string) (*openfgav1.AuthorizationModel, error) {
	ctx, span := tracer.Start(ctx, "postgres.ReadAuthorizationModel")
	defer span.End()

	return sqlcommon.ReadAuthorizationModel(ctx, sqlcommon.NewDBInfo(p.db, p.stbl, "NOW()"), store, modelID)
}

func (p *Postgres) ReadAuthorizationModels(ctx context.Context, store string, opts storage.PaginationOptions) ([]*openfgav1.AuthorizationModel, []byte, error) {
	ctx, span := tracer.Start(ctx, "postgres.ReadAuthorizationModels")
	defer span.End()

	sb := p.stbl.Select("authorization_model_id").
		Distinct().
		From("authorization_model").
		Where(sq.Eq{"store": store}).
		OrderBy("authorization_model_id desc")

	if opts.From != "" {
		token, err := sqlcommon.UnmarshallContToken(opts.From)
		if err != nil {
			return nil, nil, err
		}
		sb = sb.Where(sq.LtOrEq{"authorization_model_id": token.Ulid})
	}
	if opts.PageSize > 0 {
		sb = sb.Limit(uint64(opts.PageSize + 1)) // + 1 is used to determine whether to return a continuation token.
	}

	rows, err := sb.QueryContext(ctx)
	if err != nil {
		return nil, nil, sqlcommon.HandleSQLError(err)
	}
	defer rows.Close()

	var modelIDs []string
	var modelID string

	for rows.Next() {
		err = rows.Scan(&modelID)
		if err != nil {
			return nil, nil, sqlcommon.HandleSQLError(err)
		}

		modelIDs = append(modelIDs, modelID)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, sqlcommon.HandleSQLError(err)
	}

	var token []byte
	numModelIDs := len(modelIDs)
	if len(modelIDs) > opts.PageSize {
		numModelIDs = opts.PageSize
		token, err = json.Marshal(sqlcommon.NewContToken(modelID, ""))
		if err != nil {
			return nil, nil, err
		}
	}

	// TODO: make this concurrent with a maximum of 5 goroutines. This may be helpful:
	// https://stackoverflow.com/questions/25306073/always-have-x-number-of-goroutines-running-at-any-time
	models := make([]*openfgav1.AuthorizationModel, 0, numModelIDs)
	// We use numModelIDs here to avoid retrieving possibly one extra model.
	for i := 0; i < numModelIDs; i++ {
		model, err := p.ReadAuthorizationModel(ctx, store, modelIDs[i])
		if err != nil {
			return nil, nil, err
		}
		models = append(models, model)
	}

	return models, token, nil
}

func (p *Postgres) FindLatestAuthorizationModelID(ctx context.Context, store string) (string, error) {
	ctx, span := tracer.Start(ctx, "postgres.FindLatestAuthorizationModelID")
	defer span.End()

	var modelID string
	err := p.stbl.
		Select("authorization_model_id").
		From("authorization_model").
		Where(sq.Eq{"store": store}).
		OrderBy("authorization_model_id desc").
		Limit(1).
		QueryRowContext(ctx).
		Scan(&modelID)
	if err != nil {
		return "", sqlcommon.HandleSQLError(err)
	}

	return modelID, nil
}

func (p *Postgres) MaxTypesPerAuthorizationModel() int {
	return p.maxTypesPerModelField
}

func (p *Postgres) WriteAuthorizationModel(ctx context.Context, store string, model *openfgav1.AuthorizationModel) error {
	ctx, span := tracer.Start(ctx, "postgres.WriteAuthorizationModel")
	defer span.End()

	typeDefinitions := model.GetTypeDefinitions()

	if len(typeDefinitions) > p.MaxTypesPerAuthorizationModel() {
		return storage.ExceededMaxTypeDefinitionsLimitError(p.maxTypesPerModelField)
	}

	return sqlcommon.WriteAuthorizationModel(ctx, sqlcommon.NewDBInfo(p.db, p.stbl, "NOW()"), store, model)
}

// CreateStore is slightly different between Postgres and MySQL
func (p *Postgres) CreateStore(ctx context.Context, store *openfgav1.Store) (*openfgav1.Store, error) {
	ctx, span := tracer.Start(ctx, "postgres.CreateStore")
	defer span.End()

	var id, name string
	var createdAt time.Time
	err := p.stbl.
		Insert("store").
		Columns("id", "name", "created_at", "updated_at").
		Values(store.Id, store.Name, "NOW()", "NOW()").
		Suffix("returning id, name, created_at").
		QueryRowContext(ctx).
		Scan(&id, &name, &createdAt)
	if err != nil {
		return nil, sqlcommon.HandleSQLError(err)
	}

	return &openfgav1.Store{
		Id:        id,
		Name:      name,
		CreatedAt: timestamppb.New(createdAt),
		UpdatedAt: timestamppb.New(createdAt),
	}, nil
}

func (p *Postgres) GetStore(ctx context.Context, id string) (*openfgav1.Store, error) {
	ctx, span := tracer.Start(ctx, "postgres.GetStore")
	defer span.End()

	row := p.stbl.
		Select("id", "name", "created_at", "updated_at").
		From("store").
		Where(sq.Eq{
			"id":         id,
			"deleted_at": nil,
		}).
		QueryRowContext(ctx)

	var storeID, name string
	var createdAt, updatedAt time.Time
	err := row.Scan(&storeID, &name, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, storage.ErrNotFound
		}
		return nil, sqlcommon.HandleSQLError(err)
	}

	return &openfgav1.Store{
		Id:        storeID,
		Name:      name,
		CreatedAt: timestamppb.New(createdAt),
		UpdatedAt: timestamppb.New(updatedAt),
	}, nil
}

func (p *Postgres) ListStores(ctx context.Context, opts storage.PaginationOptions) ([]*openfgav1.Store, []byte, error) {
	ctx, span := tracer.Start(ctx, "postgres.ListStores")
	defer span.End()

	sb := p.stbl.Select("id", "name", "created_at", "updated_at").
		From("store").
		Where(sq.Eq{"deleted_at": nil}).
		OrderBy("id")

	if opts.From != "" {
		token, err := sqlcommon.UnmarshallContToken(opts.From)
		if err != nil {
			return nil, nil, err
		}
		sb = sb.Where(sq.GtOrEq{"id": token.Ulid})
	}
	if opts.PageSize > 0 {
		sb = sb.Limit(uint64(opts.PageSize + 1)) // + 1 is used to determine whether to return a continuation token.
	}

	rows, err := sb.QueryContext(ctx)
	if err != nil {
		return nil, nil, sqlcommon.HandleSQLError(err)
	}
	defer rows.Close()

	var stores []*openfgav1.Store
	var id string
	for rows.Next() {
		var name string
		var createdAt, updatedAt time.Time
		err := rows.Scan(&id, &name, &createdAt, &updatedAt)
		if err != nil {
			return nil, nil, sqlcommon.HandleSQLError(err)
		}

		stores = append(stores, &openfgav1.Store{
			Id:        id,
			Name:      name,
			CreatedAt: timestamppb.New(createdAt),
			UpdatedAt: timestamppb.New(updatedAt),
		})
	}

	if err := rows.Err(); err != nil {
		return nil, nil, sqlcommon.HandleSQLError(err)
	}

	if len(stores) > opts.PageSize {
		contToken, err := json.Marshal(sqlcommon.NewContToken(id, ""))
		if err != nil {
			return nil, nil, err
		}
		return stores[:opts.PageSize], contToken, nil
	}

	return stores, nil, nil
}

func (p *Postgres) DeleteStore(ctx context.Context, id string) error {
	ctx, span := tracer.Start(ctx, "postgres.DeleteStore")
	defer span.End()

	_, err := p.stbl.
		Update("store").
		Set("deleted_at", "NOW()").
		Where(sq.Eq{"id": id}).
		ExecContext(ctx)
	if err != nil {
		return sqlcommon.HandleSQLError(err)
	}

	return nil
}

// WriteAssertions is slightly different between Postgres and MySQL
func (p *Postgres) WriteAssertions(ctx context.Context, store, modelID string, assertions []*openfgav1.Assertion) error {
	ctx, span := tracer.Start(ctx, "postgres.WriteAssertions")
	defer span.End()

	marshalledAssertions, err := proto.Marshal(&openfgav1.Assertions{Assertions: assertions})
	if err != nil {
		return err
	}

	_, err = p.stbl.
		Insert("assertion").
		Columns("store", "authorization_model_id", "assertions").
		Values(store, modelID, marshalledAssertions).
		Suffix("ON CONFLICT (store, authorization_model_id) DO UPDATE SET assertions = ?", marshalledAssertions).
		ExecContext(ctx)
	if err != nil {
		return sqlcommon.HandleSQLError(err)
	}

	return nil
}

func (p *Postgres) ReadAssertions(ctx context.Context, store, modelID string) ([]*openfgav1.Assertion, error) {
	ctx, span := tracer.Start(ctx, "postgres.ReadAssertions")
	defer span.End()

	var marshalledAssertions []byte
	err := p.stbl.
		Select("assertions").
		From("assertion").
		Where(sq.Eq{
			"store":                  store,
			"authorization_model_id": modelID,
		}).
		QueryRowContext(ctx).
		Scan(&marshalledAssertions)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []*openfgav1.Assertion{}, nil
		}
		return nil, sqlcommon.HandleSQLError(err)
	}

	var assertions openfgav1.Assertions
	err = proto.Unmarshal(marshalledAssertions, &assertions)
	if err != nil {
		return nil, err
	}

	return assertions.Assertions, nil
}

func (p *Postgres) ReadChanges(
	ctx context.Context,
	store, objectTypeFilter string,
	opts storage.PaginationOptions,
	horizonOffset time.Duration,
) ([]*openfgav1.TupleChange, []byte, error) {
	ctx, span := tracer.Start(ctx, "postgres.ReadChanges")
	defer span.End()

	sb := p.stbl.Select("ulid", "object_type", "object_id", "relation", "user_object_type", "user_object_id", "user_relation", "operation", "inserted_at").
		From("changelog").
		Where(sq.Eq{"store": store}).
		Where(fmt.Sprintf("inserted_at < NOW() - interval '%dms'", horizonOffset.Milliseconds())).
		OrderBy("inserted_at asc")

	if objectTypeFilter != "" {
		sb = sb.Where(sq.Eq{"object_type": objectTypeFilter})
	}
	if opts.From != "" {
		token, err := sqlcommon.UnmarshallContToken(opts.From)
		if err != nil {
			return nil, nil, err
		}
		if token.ObjectType != objectTypeFilter {
			return nil, nil, storage.ErrMismatchObjectType
		}

		sb = sb.Where(sq.Gt{"ulid": token.Ulid}) // > as we always return a continuation token
	}
	if opts.PageSize > 0 {
		sb = sb.Limit(uint64(opts.PageSize)) // + 1 is NOT used here as we always return a continuation token
	}

	rows, err := sb.QueryContext(ctx)
	if err != nil {
		return nil, nil, sqlcommon.HandleSQLError(err)
	}
	defer rows.Close()

	var changes []*openfgav1.TupleChange
	var ulid string
	for rows.Next() {
		var objectType, objectID, relation, userObjectType, userObjectID, userRelation string
		var operation int
		var insertedAt time.Time

		err = rows.Scan(&ulid, &objectType, &objectID, &relation, &userObjectType, &userObjectID, &userRelation, &operation, &insertedAt)
		if err != nil {
			return nil, nil, sqlcommon.HandleSQLError(err)
		}

		changes = append(changes, &openfgav1.TupleChange{
			TupleKey: &openfgav1.TupleKey{
				Object:   tupleUtils.BuildObject(objectType, objectID),
				Relation: relation,
				User:     tupleUtils.FromUserParts(userObjectType, userObjectID, userRelation),
			},
			Operation: openfgav1.TupleOperation(operation),
			Timestamp: timestamppb.New(insertedAt.UTC()),
		})
	}

	if len(changes) == 0 {
		return nil, nil, storage.ErrNotFound
	}

	contToken, err := json.Marshal(sqlcommon.NewContToken(ulid, objectTypeFilter))
	if err != nil {
		return nil, nil, err
	}

	return changes, contToken, nil
}

// IsReady reports whether this Postgres datastore instance is ready
// to accept connections.
func (p *Postgres) IsReady(ctx context.Context) (bool, error) {
	return sqlcommon.IsReady(ctx, p.db)
}
