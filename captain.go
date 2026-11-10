package captain // import "github.com/harbur/captain"

import (
	"fmt"
	"gopkg.in/cheggaaa/pb.v1"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// Debug can be turned on to enable debug mode.
var Debug bool

// StatusError provides error code and id
type StatusError struct {
	error  error
	status int
}

type BuildOptions struct {
	Config       Config
	Tag          string
	Force        bool
	All_branches bool
	Long_sha     bool
	Branch_tags  bool
	Commit_tags  bool

	// Lifecycle is the shared cancellation scope for build/test/push.
	// When nil, a never-cancelled background lifecycle is used so that
	// standalone callers keep their historical behavior.
	Lifecycle *Lifecycle
}

// lifecycle returns the shared cancellation scope, falling back to the
// inert background lifecycle for callers that did not provide one.
func (opts BuildOptions) lifecycle() *Lifecycle {
	if opts.Lifecycle != nil {
		return opts.Lifecycle
	}
	return backgroundLifecycle
}

// Pre function executes commands on pre section before build
func Pre(app App) error {
	return runPre(backgroundLifecycle, app)
}

func runPre(lc *Lifecycle, app App) error {
	for _, value := range app.Pre {
		if err := lc.Checkpoint(); err != nil {
			return err
		}
		info("Running pre command: %s", value)
		if err := lc.Finish(executeCtx(lc.Context(), "bash", "-c", value)); err != nil {
			return err
		}
	}
	return nil
}

// Post function executes commands on pre section after build
func Post(app App) error {
	return runPost(backgroundLifecycle, app)
}

func runPost(lc *Lifecycle, app App) error {
	for _, value := range app.Post {
		if err := lc.Checkpoint(); err != nil {
			return err
		}
		info("Running post command: %s", value)
		if err := lc.Finish(executeCtx(lc.Context(), "bash", "-c", value)); err != nil {
			return err
		}
	}
	return nil
}

// tagSwallow applies a tag while honouring the lifecycle. Historically
// these particular tag calls ignored ordinary tag errors, so only a
// cancelled run is allowed to interrupt the flow.
func tagSwallow(lc *Lifecycle, app App, origin, tag string) error {
	if err := lc.Checkpoint(); err != nil {
		return err
	}
	_ = tagImageCtx(lc.Context(), app, origin, tag)
	return lc.Finish(nil)
}

// tagStrict applies a branch tag where failures historically aborted the
// run with TagFailed. Cancellation still takes precedence over the exit.
func tagStrict(lc *Lifecycle, app App, origin, tag string) error {
	if err := lc.Checkpoint(); err != nil {
		return err
	}
	if err := tagImageCtx(lc.Context(), app, origin, tag); err != nil {
		if lc.Cancelled() {
			return ErrCancelled
		}
		os.Exit(TagFailed)
	}
	return lc.Finish(nil)
}

// Build function compiles the Containers of the project
func Build(opts BuildOptions) {
	if runErr := runBuild(opts); runErr != nil {
		if runErr == ErrCancelled {
			err("Build cancelled")
			os.Exit(Cancelled)
		}
		err("Build failed: %s", runErr)
		os.Exit(BuildFailed)
	}
}

// runBuild contains the build logic and returns ErrCancelled at any
// stage boundary or step completion once the lifecycle is terminated,
// which makes it testable without killing the test process.
func runBuild(opts BuildOptions) error {
	config := opts.Config
	lc := opts.lifecycle()

	var rev = getRevision(opts.Long_sha)

	// For each App
	for _, app := range config.GetApps() {
		// Stage switch guard: never start another app after termination.
		if err := lc.Checkpoint(); err != nil {
			return err
		}

		// If no Git repo exist
		if !isGit() {
			// Perfoming [build latest]
			debug("No local git repository found, just building latest")

			// Execute Pre commands
			if res := lc.Finish(runPre(lc, app)); res != nil {
				if res == ErrCancelled {
					return res
				}
				err("Pre execution returned non-zero status")
				return res
			}

			// Build latest image
			if err := lc.Checkpoint(); err != nil {
				return err
			}
			res := lc.Finish(buildImageCtx(lc.Context(), app, "latest", opts.Force))
			if res != nil {
				if res == ErrCancelled {
					return res
				}
				os.Exit(BuildFailed)
			}

			// No tagging is allowed once cancelled.
			if err := lc.Checkpoint(); err != nil {
				return err
			}

			// Add additional user-defined Tag
			if opts.Tag != "" {
				if err := tagSwallow(lc, app, "latest", opts.Tag); err != nil {
					return err
				}
			}
		} else {
			// Skip build if there are no local changes and the commit is already built
			if !isDirty() && imageExist(app, rev) && !opts.Force {
				// Performing [skip rev|tag rev@latest|tag rev@branch]
				info("Skipping build of %s:%s - image is already built", app.Image, rev)

				// Tag commit image
				if err := tagSwallow(lc, app, rev, "latest"); err != nil {
					return err
				}

				// Tag branch image
				for _, branch := range getBranches(opts.All_branches) {
					if err := tagStrict(lc, app, rev, branch); err != nil {
						return err
					}
				}

				// Add additional user-defined Tag
				if opts.Tag != "" {
					if err := tagSwallow(lc, app, rev, opts.Tag); err != nil {
						return err
					}
				}
			} else {
				// Performing [build latest|tag latest@rev|tag latest@branch]

				// Execute Pre commands
				if res := lc.Finish(runPre(lc, app)); res != nil {
					if res == ErrCancelled {
						return res
					}
					err("Pre execution returned non-zero status")
				}

				// Build latest image
				if err := lc.Checkpoint(); err != nil {
					return err
				}
				res := lc.Finish(buildImageCtx(lc.Context(), app, "latest", opts.Force))
				if res != nil {
					if res == ErrCancelled {
						return res
					}
					os.Exit(BuildFailed)
				}
				if isDirty() {
					debug("Skipping tag of %s:%s - local changes exist", app.Image, rev)
				} else {
					// No tagging is allowed once cancelled.
					if err := lc.Checkpoint(); err != nil {
						return err
					}

					// Tag commit image
					if err := tagSwallow(lc, app, "latest", rev); err != nil {
						return err
					}

					// Tag branch image
					for _, branch := range getBranches(opts.All_branches) {
						if err := tagStrict(lc, app, "latest", branch); err != nil {
							return err
						}
					}

					// Add additional user-defined Tag
					if opts.Tag != "" {
						if err := tagSwallow(lc, app, rev, opts.Tag); err != nil {
							return err
						}
					}
				}
			}
		}

		// Execute Post commands only while the run is still alive:
		// cancellation must never trigger ordinary post commands.
		if err := lc.Checkpoint(); err != nil {
			return err
		}
		if res := lc.Finish(runPost(lc, app)); res != nil {
			if res == ErrCancelled {
				return res
			}
			err("Post execution returned non-zero status")
		}
	}
	return nil
}

// Test function executes the tests of the project
func Test(opts BuildOptions) {
	if runErr := runTest(opts); runErr != nil {
		if runErr == ErrCancelled {
			err("Test cancelled")
			os.Exit(Cancelled)
		}
		err("Test execution returned non-zero status")
		os.Exit(ExecuteFailed)
	}
}

func runTest(opts BuildOptions) error {
	config := opts.Config
	lc := opts.lifecycle()

	for _, app := range config.GetApps() {
		// Stage switch guard.
		if err := lc.Checkpoint(); err != nil {
			return err
		}
		for _, value := range app.Test {
			if err := lc.Checkpoint(); err != nil {
				return err
			}
			info("Running test command: %s", value)
			res := lc.Finish(executeCtx(lc.Context(), "bash", "-c", value))
			if res != nil {
				if res == ErrCancelled {
					return res
				}
				err("Test execution returned non-zero status")
				os.Exit(ExecuteFailed)
			}
		}
	}
	return nil
}

// Push function pushes the containers to the remote registry
func Push(opts BuildOptions) {
	if runErr := runPush(opts); runErr != nil {
		if runErr == ErrCancelled {
			err("Push cancelled")
			os.Exit(Cancelled)
		}
		err("Push returned non-zero status")
		os.Exit(ExecuteFailed)
	}
}

func runPush(opts BuildOptions) error {
	config := opts.Config
	lc := opts.lifecycle()

	// If no Git repo exist
	if !isGit() {
		err("No local git repository found, cannot push")
		os.Exit(NoGit)
	}

	if isDirty() {
		err("Git repository has local changes, cannot push")
		os.Exit(GitDirty)
	}

	for _, app := range config.GetApps() {
		// Stage switch guard: no app is pushed after termination.
		if err := lc.Checkpoint(); err != nil {
			return err
		}
		for _, branch := range getBranches(opts.All_branches) {
			if err := lc.Checkpoint(); err != nil {
				return err
			}
			info("Pushing image %s:%s", app.Image, "latest")
			if err := pushOne(lc, app.Image, "latest"); err != nil {
				return err
			}
			if opts.Branch_tags {
				info("Pushing image %s:%s", app.Image, branch)
				if err := pushOne(lc, app.Image, branch); err != nil {
					return err
				}
			}
			if opts.Commit_tags {
				rev := getRevision(opts.Long_sha)
				info("Pushing image %s:%s", app.Image, rev)
				if err := pushOne(lc, app.Image, rev); err != nil {
					return err
				}
			}

			// Add additional user-defined Tag
			if opts.Tag != "" {
				info("Pushing image %s:%s", app.Image, opts.Tag)
				if err := pushOne(lc, app.Image, opts.Tag); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// pushOne performs a single push while honouring the lifecycle. Ordinary
// push failures keep their historical ExecuteFailed exit.
func pushOne(lc *Lifecycle, image, version string) error {
	if err := lc.Checkpoint(); err != nil {
		return err
	}
	res := lc.Finish(pushImageCtx(lc.Context(), image, version))
	if res != nil {
		if res == ErrCancelled {
			return res
		}
		err("Push returned non-zero status")
		os.Exit(ExecuteFailed)
	}
	return nil
}

// Pull function pulls the containers from the remote registry
func Pull(opts BuildOptions) {
	config := opts.Config

	for _, app := range config.GetApps() {
		for _, branch := range getBranches(opts.All_branches) {
			info("Pulling image %s:%s", app.Image, "latest")
			if res := pullImage(app.Image, "latest"); res != nil {
				err("Pull returned non-zero status")
				os.Exit(ExecuteFailed)
			}
			if opts.Branch_tags {
				info("Pulling image %s:%s", app.Image, branch)
				if res := pullImage(app.Image, branch); res != nil {
					err("Pull returned non-zero status")
					os.Exit(ExecuteFailed)
				}
			}
			if opts.Commit_tags {
				rev := getRevision(opts.Long_sha)
				info("Pulling image %s:%s", app.Image, rev)
				if res := pullImage(app.Image, rev); res != nil {
					err("Pull returned non-zero status")
					os.Exit(ExecuteFailed)
				}
			}

			// Add additional user-defined Tag
			if opts.Tag != "" {
				info("Pulling image %s:%s", app.Image, opts.Tag)
				if res := pullImage(app.Image, opts.Tag); res != nil {
					err("Pull returned non-zero status")
					os.Exit(ExecuteFailed)
				}
			}
		}
	}
}

// Purge function purges the stale images
func Purge(opts BuildOptions) {
	config := opts.Config

	// For each App
	for _, app := range config.GetApps() {
		var tags = []string{}

		// Retrieve the list of the existing Image tags
		for _, img := range getImages(app) {
			tags = append(tags, img.RepoTags...)
		}

		// Remove from the list: The latest image
		for key, tag := range tags {
			if tag == app.Image+":latest" {
				tags = append(tags[:key], tags[key+1:]...)
			}
		}

		// Remove from the list: The current commit-id
		for key, tag := range tags {
			if tag == app.Image+":"+getRevision(opts.Long_sha) {
				tags = append(tags[:key], tags[key+1:]...)
			}
		}

		// Remove from the list: The working-dir git branches
		for _, branch := range getBranches(opts.All_branches) {
			for key, tag := range tags {
				if tag == app.Image+":"+branch {
					tags = append(tags[:key], tags[key+1:]...)
				}
			}
		}

		// Proceed with deletion of Images
		for _, tag := range tags {
			info("Deleting image %s", tag)
			res := removeImage(tag)
			if res != nil {
				err("Deleting image failed: %s", res)
				os.Exit(DeleteImageFailed)
			}
		}
	}
}

func SelfUpdate() {
	captainDir := filepath.FromSlash(os.Getenv("HOME") + "/.captain")
	binariesDir := filepath.FromSlash(captainDir + "/binaries")
	binDir := filepath.FromSlash(captainDir + "/bin")

	kernel := runtime.GOOS
	arch := runtime.GOARCH
	captainSymlinkPath := filepath.FromSlash(binDir + "/captain")
	currentVersionPath, _ := os.Readlink(captainSymlinkPath)

	info("Checking the last version of Captain...")
	version := findLastVersion()
	downloadUrl := fmt.Sprintf("https://github.com/harbur/captain/releases/download/%s/captain_%s_%s", version, kernel, arch)
	downloadedVersionPath := filepath.FromSlash(binariesDir + "/captain-" + version)

	if currentVersionPath == downloadedVersionPath {
		info("You have installed the last version of captain (%s)", version)
		return
	}

	info("New version available, start downloading %s", version)

	// create binaries dir
	if err := os.MkdirAll(binariesDir, 0755); err != nil {
		fmt.Println(err)
	}

	// download new version
	if err := downloadFile(downloadedVersionPath, downloadUrl); err != nil {
		fmt.Println(err)
	}

	// grant excution to download version
	if err := os.Chmod(downloadedVersionPath, 0755); err != nil {
		fmt.Println(err)
	}

	info("Downloaded captain %s", version)

	if err := os.MkdirAll(binDir, 0755); err != nil {
		fmt.Println(err)
	}

	if _, err := os.Stat(captainSymlinkPath); err == nil {
		os.Remove(captainSymlinkPath)
	}

	if err := os.Symlink(downloadedVersionPath, captainSymlinkPath); err != nil {
		fmt.Println(err)
	}

	info("Installed captain %s", version)
}

func findLastVersion() string {
	url := "https://raw.githubusercontent.com/harbur/captain/master/VERSION"

	res, err := http.Get(url)

	if err != nil {
		panic(err)
	}
	defer res.Body.Close()

	content, err := ioutil.ReadAll(res.Body)
	if err != nil {
		panic(err)
	}

	return string(content)
}

func downloadFile(filepath string, url string) error {

	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// create and start bar
	bar := pb.New64(resp.ContentLength).SetUnits(pb.U_BYTES).Start()
	defer bar.Finish()

	// create proxy reader
	reader := bar.NewProxyReader(resp.Body)

	// and copy from pb reader
	_, err = io.Copy(out, reader)
	if err != nil {
		return err
	}

	return nil
}
