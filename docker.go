package captain // import "github.com/harbur/captain"

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fsouza/go-dockerclient"
)

const defaultEndpoint = "unix:///var/run/docker.sock"

var client *docker.Client

func init() {
	var err error
	client, err = docker.NewClientFromEnv()
	if err != nil {
		panic(err)
	}
}

type BuildArgSet struct {
	slice []docker.BuildArg
}

func buildImage(app App, tag string, force bool) error {
	return buildImageCtx(context.Background(), app, tag, force)
}

// buildImageCtx runs a Docker build bound to ctx. The streaming build
// request carries ctx, so cancelling the lifecycle closes the Docker
// connection and unblocks the build wait immediately.
func buildImageCtx(ctx context.Context, app App, tag string, force bool) error {
	return buildImageWithClient(ctx, client, app, tag, force)
}

// buildImageWithClient is the testable core of buildImageCtx; callers can
// inject a client pointing at a fake streaming endpoint.
func buildImageWithClient(ctx context.Context, c *docker.Client, app App, tag string, force bool) error {
	info("Building image %s:%s", app.Image, tag)

	// Nasty issue with CircleCI https://github.com/docker/docker/issues/4897
	if os.Getenv("CIRCLECI") == "true" {
		info("Running at %s environment...", "CIRCLECI")
		return executeCtx(ctx, "docker", "build", "-t", app.Image+":"+tag, filepath.Dir(app.Build))
	}

	// Create BuildArg set
	buildArgSet := BuildArgSet{(make([]docker.BuildArg, 0, 10))}
	if len(app.Build_arg) > 0 {
		for k, arg := range app.Build_arg {
			buildArgSet.slice = append(buildArgSet.slice, docker.BuildArg{Name: k, Value: arg})
		}
	}
	opts := docker.BuildImageOptions{
		Name:                app.Image + ":" + tag,
		Dockerfile:          filepath.Base(app.Build),
		NoCache:             force,
		SuppressOutput:      false,
		RmTmpContainer:      true,
		ForceRmTmpContainer: true,
		OutputStream:        os.Stdout,
		ContextDir:          filepath.Dir(app.Build),
		BuildArgs:           buildArgSet.slice,
		// Thread the shared lifecycle into the streaming HTTP request:
		// go-dockerclient closes the underlying connection once it is
		// cancelled, ending an in-flight build instead of hanging.
		Context: ctx,
	}

	// Use ~/.docker/ auth configuration if exists
	dockercfg, _ := docker.NewAuthConfigurationsFromDockerCfg()
	if dockercfg != nil {
		opts.AuthConfigs = *dockercfg
	}

	err := c.BuildImage(opts)
	if err != nil {
		// Context-driven cancellations are an expected shutdown path,
		// not a build failure worth printing as one.
		if err != context.Canceled && err != context.DeadlineExceeded {
			fmt.Printf("%s", err)
		}
	}
	return err
}

func pushImage(image string, version string) error {
	return pushImageCtx(context.Background(), image, version)
}

// pushImageCtx binds the docker CLI process tree to ctx so a cancelled
// lifecycle interrupts pushes instead of letting them run to completion.
func pushImageCtx(ctx context.Context, image string, version string) error {
	return executeCtx(ctx, "docker", "push", image+":"+version)
}

func pullImage(image string, version string) error {
	return pullImageCtx(context.Background(), image, version)
}

// pullImageCtx mirrors pushImageCtx for image pulls.
func pullImageCtx(ctx context.Context, image string, version string) error {
	return executeCtx(ctx, "docker", "pull", image+":"+version)
}

func tagImage(app App, origin string, tag string) error {
	return tagImageCtx(context.Background(), app, origin, tag)
}

// tagImageCtx performs the tag through a context-bound Docker request,
// letting the lifecycle reject tags issued after cancellation.
func tagImageCtx(ctx context.Context, app App, origin string, tag string) error {
	if tag != "" {
		info("Tagging image %s:%s as %s:%s", app.Image, origin, app.Image, tag)
		opts := docker.TagImageOptions{Repo: app.Image, Tag: tag, Force: true, Context: ctx}
		err := client.TagImage(app.Image+":"+origin, opts)
		if err != nil {
			if err != context.Canceled && err != context.DeadlineExceeded {
				fmt.Printf("%s", err)
			}
		}
		return err
	}

	debug("Skipping tag of %s - no git repository", app.Image)

	return nil
}

func removeImage(name string) error {
	return client.RemoveImage(name)
}

/**
 * Retrieves a list of existing Images for the specific App.
 */
func getImages(app App) []docker.APIImages {
	debug("Getting images %s", app.Image)
	imgs, _ := client.ListImages(docker.ListImagesOptions{All: false, Filter: app.Image})
	return imgs
}

func imageExist(app App, tag string) bool {
	repo := app.Image + ":" + tag
	image, _ := client.InspectImage(repo)
	if image != nil {
		return true
	}
	return false
}
