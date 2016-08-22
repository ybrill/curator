package sthree

import (
	"errors"
	"strings"
	"testing"

	"github.com/goamz/goamz/s3"
	"github.com/mongodb/amboy"
	"github.com/mongodb/amboy/dependency"
	"github.com/stretchr/testify/suite"
)

// BucketJobSuite collects tests of the amboy.Job implementations that
// support syncing files to and from S3. See BucketSuite for
// integration tests from a high level, and SyncFromSuite and
// SyncToSuite for more narrowly scoped checks of the behavior of
// single file/object sync operations.
type BucketJobSuite struct {
	fromJob    *syncFromJob
	toJob      *syncToJob
	bucket     *Bucket
	jobs       []amboy.Job
	withDelete bool
	suite.Suite
}

func TestBucketJobSuiteWithoutDelete(t *testing.T) {
	t.Parallel()
	suite.Run(t, &BucketJobSuite{
		withDelete: false,
	})
}

func TestBucketJobSuiteWithDelete(t *testing.T) {
	t.Parallel()
	suite.Run(t, &BucketJobSuite{
		withDelete: true,
	})
}

func (s *BucketJobSuite) SetupSuite() {
	s.bucket = GetBucket("build-test-curator")
	s.NoError(s.bucket.Open())
}

func (s *BucketJobSuite) SetupTest() {
	s.toJob = newSyncToJob(s.bucket, "local-file-name", s3.Key{Key: "remote-file-name"}, s.withDelete)
	s.fromJob = newSyncFromJob(s.bucket, "local-file-name", s3.Key{}, s.withDelete)
	s.jobs = []amboy.Job{s.toJob, s.fromJob}
}

func (s *BucketJobSuite) TearDownSuite() {
	s.bucket.Close()
}

func (s *BucketJobSuite) TestSyncJobsImplementInterface() {
	job := (*amboy.Job)(nil)

	// test that the objects theme selves are correct
	s.Implements(job, new(syncFromJob))
	s.Implements(job, new(syncToJob))

	// test that the job constructors produce valid implementations
	for _, syncJob := range s.jobs {
		s.Implements(job, syncJob)
	}
}

func (s *BucketJobSuite) TestSyncJobCorrectlyStoresFileNames() {
	s.Equal("local-file-name", s.toJob.localPath)
	s.Equal("local-file-name", s.fromJob.localPath)
	s.Equal("remote-file-name", s.toJob.remoteFile.Key)
}

func (s *BucketJobSuite) TestSyncJobsHaveExpectedJobTypes() {
	s.Equal(0, s.fromJob.Type().Version)
	s.Equal(0, s.toJob.Type().Version)

	s.Equal("s3-sync-from", s.fromJob.Type().Name)
	s.Equal("s3-sync-to", s.toJob.Type().Name)
}

func (s *BucketJobSuite) TestSyncJobsHaveWellFormedName() {
	strings.HasSuffix(s.fromJob.ID(), "sync-from")
	strings.HasSuffix(s.toJob.ID(), "sync-to")
}

func (s *BucketJobSuite) TestSyncJobsAreIncompleteByDefault() {
	for _, job := range s.jobs {
		s.False(job.Completed())
	}
}

func (s *BucketJobSuite) TestSyncJobsHaveStaticDepenendecyManagers() {
	always := dependency.NewAlways()
	creates := dependency.NewCreatesFileInstance()

	for _, job := range s.jobs {
		s.Equal(job.Dependency(), always)
		job.SetDependency(creates)
		s.Equal(job.Dependency(), always)
	}
}

func (s *BucketJobSuite) TestMarkCompleteMethodChangesCompleteState() {
	s.False(s.fromJob.Completed())
	s.False(s.toJob.Completed())

	s.fromJob.markComplete()
	s.toJob.markComplete()

	s.True(s.fromJob.Completed())
	s.True(s.toJob.Completed())
}

func (s *BucketJobSuite) TestAddErrorDoesNotPersistNilErrors() {
	var err error

	s.Len(s.fromJob.errors, 0)
	s.Len(s.toJob.errors, 0)

	s.NoError(s.fromJob.Error())
	s.NoError(s.toJob.Error())

	s.fromJob.addError(err)
	s.toJob.addError(err)

	s.Len(s.fromJob.errors, 0)
	s.Len(s.toJob.errors, 0)

	s.NoError(s.fromJob.Error())
	s.NoError(s.toJob.Error())
}

func (s *BucketJobSuite) AddErrorsDoesPersistErrors() {
	err := errors.New("test")

	s.Len(s.fromJob.errors, 0)
	s.Len(s.toJob.errors, 0)

	s.NoError(s.fromJob.Error())
	s.NoError(s.toJob.Error())

	s.fromJob.addError(err)
	s.toJob.addError(err)

	s.Len(s.fromJob.errors, 1)
	s.Len(s.toJob.errors, 1)

	s.Error(s.fromJob.Error())
	s.Error(s.toJob.Error())
}

func (s *BucketJobSuite) TestErrorMethodDoesNotImpactInternalErrorState() {
	err := errors.New("test")

	s.Len(s.fromJob.errors, 0)
	s.Len(s.toJob.errors, 0)

	s.NoError(s.fromJob.Error())
	s.NoError(s.toJob.Error())

	s.fromJob.addError(err)
	s.toJob.addError(err)

	for i := 1; i < 20; i++ {
		s.Len(s.fromJob.errors, 1)
		s.Len(s.toJob.errors, 1)

		s.Error(s.fromJob.Error())
		s.Error(s.toJob.Error())
	}
}
