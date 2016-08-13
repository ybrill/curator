package repobuilder

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/goamz/goamz/s3"
	"github.com/mongodb/amboy"
	"github.com/mongodb/amboy/dependency"
	"github.com/mongodb/curator"
	"github.com/mongodb/curator/sthree"
	"github.com/pkg/errors"
	"github.com/tychoish/grip"
)

type jobImpl interface {
	rebuildRepo(string, *sync.WaitGroup)
	injectPackage(string, string) ([]string, error)
}

type Job struct {
	Name         string                `bson:"name" json:"name" yaml:"name"`
	Distro       *RepositoryDefinition `bson:"distro" json:"distro" yaml:"distro"`
	Conf         *RepositoryConfig     `bson:"conf" json:"conf" yaml:"conf"`
	DryRun       bool                  `bson:"dry_run" json:"dry_run" yaml:"dry_run"`
	IsComplete   bool                  `bson:"completed" json:"completed" yaml:"completed"`
	Output       map[string]string     `bson:"output" json:"output" yaml:"output"`
	JobType      amboy.JobType         `bson:"job_type" json:"job_type" yaml:"job_type"`
	D            dependency.Manager    `bson:"dependency" json:"dependency" yaml:"dependency"`
	Version      string                `bson:"version" json:"version" yaml:"version"`
	Arch         string                `bson:"arch" json:"arch" yaml:"arch"`
	Profile      string                `bson:"aws_profile" json:"aws_profile" yaml:"aws_profile"`
	WorkSpace    string                `bson:"local_workdir" json:"local_workdir" yaml:"local_workdir"`
	PackagePaths []string              `bson:"package_paths" json:"package_paths" yaml:"package_paths"`
	Errors       []error               `bson:"errors" json:"errors" yaml:"errors"`
	workingDirs  []string
	release      *curator.MongoDBVersion
	mutex        sync.RWMutex
}

func buildRepoJob() *Job {
	return &Job{
		D:      dependency.NewAlways(),
		Output: make(map[string]string),
		JobType: amboy.JobType{
			Name:    "build-repo",
			Version: 0,
		},
	}
}

func (j *Job) ID() string {
	return j.Name
}

func (j *Job) Completed() bool {
	return j.IsComplete
}

func (j *Job) Type() amboy.JobType {
	return j.JobType
}

func (j *Job) Dependency() dependency.Manager {
	return j.D
}

func (j *Job) SetDependency(d dependency.Manager) {
	if d.Type().Name == dependency.AlwaysRun {
		j.D = d
	} else {
		grip.Warning("repo building jobs should take 'always'-run dependencies.")
	}
}

func (j *Job) markComplete() {
	j.IsComplete = true
}

func (j *Job) addError(err error) {
	if err != nil {
		j.mutex.Lock()
		defer j.mutex.Unlock()

		j.Errors = append(j.Errors, err)
	}
}

func (j *Job) hasErrors() bool {
	j.mutex.RLock()
	defer j.mutex.RUnlock()

	if len(j.Errors) == 0 {
		return false
	}

	return true
}

func (j *Job) Error() error {
	j.mutex.RLock()
	defer j.mutex.RUnlock()

	if len(j.Errors) == 0 {
		return nil
	}

	var outputs []string

	for _, err := range j.Errors {
		outputs = append(outputs, fmt.Sprintf("%+v", err))
	}

	return errors.New(strings.Join(outputs, "\n"))
}

func (j *Job) linkPackages(dest string) error {
	catcher := grip.NewCatcher()
	wg := &sync.WaitGroup{}
	defer wg.Wait()
	for _, pkg := range j.PackagePaths {

		if j.Distro.Type == DEB && !strings.HasSuffix(pkg, ".deb") {
			// the Packages files generated by the compile
			// task are caught in this glob. It's
			// harmless, as we regenerate these files
			// later, but just to be careful and more
			// clear, we should skip these files.
			continue
		}

		if _, err := os.Stat(dest); os.IsNotExist(err) {
			grip.Noticeln("creating directory:", dest)
			catcher.Add(os.MkdirAll(dest, 0744))
		}

		mirror := filepath.Join(dest, filepath.Base(pkg))

		if _, err := os.Stat(mirror); os.IsNotExist(err) {
			grip.Infof("copying package %s to local staging %s", pkg, dest)

			err = os.Link(pkg, mirror)
			if err != nil {
				catcher.Add(err)
				grip.Error(err)
				continue
			}

			if j.Distro.Type == RPM {
				wg.Add(1)
				go func(toSign string) {
					// sign each package, overwriting the package with the signed package.
					catcher.Add(j.signFile(toSign, "", true)) // (name, extension, overwrite)
					wg.Done()
				}(mirror)
			}

		} else {
			grip.Infof("file %s is already mirrored", mirror)
		}
	}

	return catcher.Resolve()
}

func (j *Job) injectNewPackages(local string) ([]string, error) {
	catcher := grip.NewCatcher()
	var changedRepos []string

	if j.release.IsDevelopmentBuild() || j.release.IsReleaseCandidate() {
		// nightlies and release candidates go into the testing repo:

		changed, err := injectPackage(j, local, "testing")
		catcher.Add(err)
		changedRepos = append(changedRepos, changed...)
	} else {
		// all releases (not RCs, captured above,) either dev
		// or stable go somewhere else...

		// there are repos for each series:
		changed, err := injectPackage(j, local, j.release.Series())
		catcher.Add(err)
		changedRepos = append(changedRepos, changed...)

		// all stable releases go into a special "stable"
		// series so people can upgrade from one stable branch
		// to the next seamlessly (this might be an
		// anti-pattern, but it's established.)
		if j.release.IsStableSeries() {
			changed, err = injectPackage(j, local, "stable")
			catcher.Add(err)
			changedRepos = append(changedRepos, changed...)
		}

		// all development releases go into a special unstable repo.
		if j.release.IsDevelopmentSeries() {
			changed, err = injectPackage(j, local, "unstable")
			catcher.Add(err)
			changedRepos = append(changedRepos, changed...)
		}
	}

	return changedRepos, catcher.Resolve()
}

// signFile wraps the python notary-client.py script. Pass it the name
// of a file to sign, the "archiveExtension" (which only impacts
// non-package files, as defined by the notary service and client,)
// and an "overwrite" bool. Overwrite: forces package signing to
// overwrite the existing file, removing the archive's
// signature. Using overwrite=true and a non-nil string is not logical
// and returns a warning, but is passed to the client.
func (j *Job) signFile(fileName, archiveExtension string, overwrite bool) error {
	// In the future it would be nice if we could talk to the
	// notary service directly rather than shelling out here. The
	// final option controls if we overwrite this file.

	var keyName string
	var token string

	if j.Distro.Type == DEB && j.release.Series() == "3.0" {
		keyName = "richard"
		token = os.Getenv("NOTARY_TOKEN_DEB_LEGACY")
	} else {
		keyName = "server-" + j.release.StableReleaseSeries()
		token = os.Getenv("NOTARY_TOKEN")
	}

	if token == "" {
		return errors.New(fmt.Sprintln("the notary service auth token",
			"(NOTARY_TOKEN) is not defined in the environment"))
	}

	args := []string{
		"notary-client.py",
		"--key-name", keyName,
		"--auth-token", token,
		"--comment", "\"curator package signing\"",
		"--notary-url", j.Conf.Services.NotaryURL,
		"--archive-file-ext", archiveExtension,
		"--outputs", "sig",
	}

	grip.AlertWhenf(strings.HasPrefix(archiveExtension, "."),
		"extension '%s', has a leading dot, which is almost certainly undesirable.", archiveExtension)

	grip.AlertWhenln(overwrite && len(archiveExtension) != 0,
		"specified overwrite with an archive extension:", archiveExtension,
		"this is probably an error, (not impacting packages,) but is passed to the client.")

	if overwrite {
		grip.Noticef("overwriting existing contents of file '%s' while signing it", fileName)
		args = append(args, "--package-file-suffix", "")
	} else {
		// if we're not overwriting the unsigned source file
		// with the signed file, then we should remove the
		// signed artifact before. Unclear if this is needed,
		// the cronjob did this.
		grip.CatchWarning(os.Remove(fileName + "." + archiveExtension))
	}

	args = append(args, filepath.Base(fileName))
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = filepath.Dir(fileName)

	grip.Infoln("running notary command:", strings.Replace(
		strings.Join(cmd.Args, " "),
		token, "XXXXX", -1))

	out, err := cmd.CombinedOutput()
	output := string(out)

	if err != nil {
		grip.Warningf("error signed file '%s': %s", fileName, err.Error())
	} else {
		grip.Noticeln("successfully signed file:", fileName)
	}

	grip.Debugln("notary-client.py output:", output)

	j.mutex.Lock()
	defer j.mutex.Unlock()
	j.Output["sign-"+fileName] = output

	return err
}

func (j *Job) Run() {
	bucket := sthree.GetBucketWithProfile(j.Distro.Bucket, j.Profile)
	bucket.NewFilePermission = s3.PublicRead

	err := bucket.Open()
	defer bucket.Close()
	if err != nil {
		j.addError(errors.Wrapf(err, "opening bucket %s", bucket))
		return
	}

	defer j.markComplete()
	wg := &sync.WaitGroup{}

	for _, remote := range j.Distro.Repos {
		wg.Add(1)
		go func(repo *RepositoryDefinition, workSpace, remote string) {
			grip.Infof("rebuilding %s.%s", bucket, remote)
			defer wg.Done()

			local := filepath.Join(workSpace, remote)

			var err error
			j.workingDirs = append(j.workingDirs, local)

			err = os.MkdirAll(local, 0755)
			if err != nil {
				j.addError(errors.Wrapf(err, "creating directory %s", local))
				return
			}

			if j.DryRun {
				grip.Noticef("in dry run mode. would download from %s to %s", remote, local)
			} else {
				grip.Infof("downloading from %s to %s", remote, local)
				err = bucket.SyncFrom(local, remote)
				if err != nil {
					j.addError(errors.Wrapf(err, "sync from %s to %s", remote, local))
					return
				}
			}

			var changedRepos []string
			if j.DryRun {
				grip.Noticef("in dry run mode. would link packages [%s] to %s",
					strings.Join(j.PackagePaths, "; "), local)
			} else {
				grip.Info("copying new packages into local staging area")
				changedRepos, err = j.injectNewPackages(local)
				if err != nil {
					j.addError(errors.Wrap(err, "copying packages into staging repos"))
					return
				}
			}

			rWg := &sync.WaitGroup{}
			for _, dir := range changedRepos {
				rWg.Add(1)
				go rebuildRepo(j, dir, rWg)
			}
			rWg.Wait()

			if j.hasErrors() {
				// concern: the underlying structure for hasErrors is shared,
				// so its possible that this error message will be erroneous,
				// or duplicated, if one succeeds and one fails. This shouldn't
				// be a huge issue in practice, particularly since we upload all
				// repos together.
				grip.Errorf("encountered error rebuilding %s (%s). Uploading no data",
					remote, local)
				return
			}

			if j.DryRun {
				grip.Noticef("in dry run mode. otherwise would have built %s (%s)",
					remote, local)
			} else {
				err = bucket.SyncTo(local, remote)
				if err != nil {
					j.addError(errors.Wrapf(err, "sync %s to %s", local, remote))
					return

				}
				grip.Noticef("completed rebuilding repo %s (%s)", remote, local)
			}
		}(j.Distro, j.WorkSpace, remote)
	}
	wg.Wait()

	if j.hasErrors() {
		grip.Warning("encountered error rebuilding repositories. operation complete.")
		return
	}
	grip.Notice("completed rebuilding all repositories")
}

// shim methods so that we can reuse the Run() method from
// repobuilder.Job for all types
func injectPackage(j interface{}, local, repoName string) ([]string, error) {
	switch j := j.(type) {
	case *Job:
		if j.Type().Name == "build-deb-repo" {
			job := BuildDEBRepoJob{j}
			return job.injectPackage(local, repoName)
		} else if j.Type().Name == "build-rpm-repo" {
			job := BuildRPMRepoJob{j}
			return job.injectPackage(local, repoName)
		} else {
			return []string{}, fmt.Errorf("builder %s is not supported", j.Type().Name)
		}
	default:
		return []string{}, fmt.Errorf("type %T is not supported", j)
	}
}

func rebuildRepo(j interface{}, workingDir string, wg *sync.WaitGroup) {
	grip.Infoln("rebuilding repo:", workingDir)

	switch j := j.(type) {
	case *Job:
		if j.Type().Name == "build-deb-repo" {
			job := BuildDEBRepoJob{j}
			job.rebuildRepo(workingDir, wg)
		} else if j.Type().Name == "build-rpm-repo" {
			job := BuildRPMRepoJob{j}
			job.rebuildRepo(workingDir, wg)
		} else {
			e := fmt.Sprintf("builder %s is not supported", j.Type().Name)
			grip.Error(e)
			j.addError(errors.New(e))
		}
	default:
		e := fmt.Sprintf("cannot build repo for type: %T", j)
		grip.ErrorPanic(e)
	}
}
