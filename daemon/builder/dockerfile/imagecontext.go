package dockerfile

import (
	"context"
	"runtime"

	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/moby/moby/v2/daemon/builder"
	dockerimage "github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/server/backend"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type getAndMountFunc func(context.Context, string, bool, *ocispec.Platform) (builder.Image, builder.ROLayer, error)

// imageSources mounts images and provides a cache for mounted images. It tracks
// all images so they can be unmounted at the end of the build.
type imageSources struct {
	byImageID map[string]*imageMount
	mounts    []*imageMount
	getImage  getAndMountFunc
}

func newImageSources(options builderOptions) *imageSources {
	getAndMount := func(ctx context.Context, idOrRef string, localOnly bool, platform *ocispec.Platform) (builder.Image, builder.ROLayer, error) {
		pullOption := backend.PullOptionNoPull
		if !localOnly {
			if options.Options.PullParent {
				pullOption = backend.PullOptionForcePull
			} else {
				pullOption = backend.PullOptionPreferLocal
			}
		}
		return options.Backend.GetImageAndReleasableLayer(ctx, idOrRef, backend.GetImageAndLayerOptions{
			PullOption: pullOption,
			AuthConfig: options.Options.AuthConfigs,
			Output:     options.ProgressWriter.Output,
			Platform:   platform,
		})
	}

	return &imageSources{
		byImageID: make(map[string]*imageMount),
		getImage:  getAndMount,
	}
}

func (m *imageSources) Get(ctx context.Context, idOrRef string, localOnly bool, platform *ocispec.Platform) (*imageMount, error) {
	if im, ok := m.byImageID[idOrRef]; ok {
		return im, nil
	}

	image, layer, err := m.getImage(ctx, idOrRef, localOnly, platform)
	if err != nil {
		return nil, err
	}
	im := newImageMount(image, layer)
	m.Add(im, platform)
	return im, nil
}

func (m *imageSources) Unmount() (retErr error) {
	for _, im := range m.mounts {
		if err := im.unmount(); err != nil {
			log.G(context.TODO()).Error(err)
			retErr = err
		}
	}
	return retErr
}

func (m *imageSources) Add(im *imageMount, platform *ocispec.Platform) {
	switch im.image {
	case nil:
		// Set the platform for scratch images
		if platform == nil {
			p := platforms.DefaultSpec()
			platform = &p
		}

		// Windows does not support scratch except for LCOW
		os := platform.OS
		if runtime.GOOS == "windows" {
			os = "linux"
		}

		im.image = &dockerimage.Image{V1Image: dockerimage.V1Image{
			OS:           os,
			Architecture: platform.Architecture,
			Variant:      platform.Variant,
		}}
	default:
		m.byImageID[im.image.ImageID()] = im
	}
	m.mounts = append(m.mounts, im)
}

// imageMount is a reference to an image that can be used as a builder.Source
type imageMount struct {
	image builder.Image
	layer builder.ROLayer
}

func newImageMount(image builder.Image, layer builder.ROLayer) *imageMount {
	im := &imageMount{image: image, layer: layer}
	return im
}

func (im *imageMount) unmount() error {
	if im.layer == nil {
		return nil
	}
	if err := im.layer.Release(); err != nil {
		return errors.Wrapf(err, "failed to unmount previous build image %s", im.image.ImageID())
	}
	im.layer = nil
	return nil
}

func (im *imageMount) Image() builder.Image {
	return im.image
}

func (im *imageMount) NewRWLayer() (builder.RWLayer, error) {
	return im.layer.NewRWLayer()
}

func (im *imageMount) ImageID() string {
	return im.image.ImageID()
}
