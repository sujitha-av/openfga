package test

import (
	"testing"

	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/storage"
)

func RunAllTests(t *testing.T, ds storage.OpenFGADatastore, s *server.Server) {
	RunQueryTests(t, ds, s)
	RunCommandTests(t, ds, s)
}

func RunQueryTests(t *testing.T, ds storage.OpenFGADatastore, s *server.Server) {
	t.Run("TestReadAuthorizationModelQueryErrors", func(t *testing.T) { TestReadAuthorizationModelQueryErrors(t, s) })
	t.Run("TestSuccessfulReadAuthorizationModelQuery", func(t *testing.T) { TestSuccessfulReadAuthorizationModelQuery(t, ds, s) })
	t.Run("TestReadAuthorizationModel", func(t *testing.T) { ReadAuthorizationModelTest(t, s) })
	t.Run("TestExpandQuery", func(t *testing.T) { TestExpandQuery(t, ds) })
	t.Run("TestExpandQueryErrors", func(t *testing.T) { TestExpandQueryErrors(t, ds) })

	t.Run("TestGetStoreQuery", func(t *testing.T) { TestGetStoreQuery(t, s) })
	t.Run("TestGetStoreSucceeds", func(t *testing.T) { TestGetStoreSucceeds(t, ds) })
	t.Run("TestListStores", func(t *testing.T) { TestListStores(t, ds) })

	t.Run("TestReadAssertionQuery", func(t *testing.T) { TestReadAssertionQuery(t, s) })
	t.Run("TestReadQuerySuccess", func(t *testing.T) { ReadQuerySuccessTest(t, ds) })
	t.Run("TestReadQueryError", func(t *testing.T) { ReadQueryErrorTest(t, ds) })
	t.Run("TestReadAllTuples", func(t *testing.T) { ReadAllTuplesTest(t, ds) })
	t.Run("TestReadAllTuplesInvalidContinuationToken", func(t *testing.T) { ReadAllTuplesInvalidContinuationTokenTest(t, ds) })

	t.Run("TestReadAuthorizationModelsWithoutPaging",
		func(t *testing.T) { TestReadAuthorizationModelsWithoutPaging(t, s) },
	)

	t.Run("TestReadAuthorizationModelsWithPaging",
		func(t *testing.T) { TestReadAuthorizationModelsWithPaging(t, s) },
	)

	t.Run("TestReadAuthorizationModelsInvalidContinuationToken",
		func(t *testing.T) { TestReadAuthorizationModelsInvalidContinuationToken(t, s) },
	)

	t.Run("TestReadChanges", func(t *testing.T) { TestReadChanges(t, ds) })
	t.Run("TestReadChangesReturnsSameContTokenWhenNoChanges",
		func(t *testing.T) { TestReadChangesReturnsSameContTokenWhenNoChanges(t, ds) },
	)
	t.Run("TestReadChangesAfterConcurrentWritesReturnsUniqueResults",
		func(t *testing.T) { TestReadChangesAfterConcurrentWritesReturnsUniqueResults(t, ds) },
	)

	t.Run("TestListObjects", func(t *testing.T) { TestListObjects(t, ds) })
	t.Run("TestReverseExpand", func(t *testing.T) { TestReverseExpand(t, ds) })

	t.Run("TestWriteAndReadAssertions", func(t *testing.T) { TestWriteAndReadAssertions(t, s) })
	t.Run("TestWriteAssertionsFailure", func(t *testing.T) { TestWriteAssertionsFailure(t, s) })
}

func RunCommandTests(t *testing.T, ds storage.OpenFGADatastore, s *server.Server) {
	t.Run("TestWriteCommand", func(t *testing.T) { TestWriteCommand(t, s) })
	t.Run("TestWriteAuthorizationModel", func(t *testing.T) { WriteAuthorizationModelTest(t, ds, s) })
	t.Run("TestCreateStore", func(t *testing.T) { TestCreateStore(t, s) })
	t.Run("TestDeleteStore", func(t *testing.T) { TestDeleteStore(t, s) })
}

func RunAllBenchmarks(b *testing.B, ds storage.OpenFGADatastore) {
	b.Run("BenchmarkListObjects", func(b *testing.B) { BenchmarkListObjects(b, ds) })
}
