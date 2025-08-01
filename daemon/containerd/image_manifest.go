package containerd

import (
	"context"
	"encoding/json"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/util/attestation"
	"github.com/moby/moby/v2/errdefs"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

var (
	errNotManifestOrIndex = errdefs.InvalidParameter(errors.New("descriptor is neither a manifest or index"))
	errNotManifest        = errdefs.InvalidParameter(errors.New("descriptor isn't a manifest"))
)

// walkImageManifests calls the handler for each locally present manifest in
// the image.
func (i *ImageService) walkImageManifests(ctx context.Context, img c8dimages.Image, handler func(img *ImageManifest) error) error {
	desc := img.Target

	handleManifest := func(ctx context.Context, d ocispec.Descriptor) error {
		platformImg, err := i.NewImageManifest(ctx, img, d)
		if err != nil {
			if errors.Is(err, errNotManifest) {
				return nil
			}
			return err
		}
		return handler(platformImg)
	}

	if c8dimages.IsManifestType(desc.MediaType) {
		return handleManifest(ctx, desc)
	}

	if c8dimages.IsIndexType(desc.MediaType) {
		return i.walkPresentChildren(ctx, desc, handleManifest)
	}

	return errors.Wrapf(errNotManifestOrIndex, "error walking manifest for %s", img.Name)
}

// walkReachableImageManifests calls the handler for each manifest in the
// multiplatform image that can be reached from the given image.
// The image might not be present locally, but its descriptor is known.
func (i *ImageService) walkReachableImageManifests(ctx context.Context, img c8dimages.Image, handler func(img *ImageManifest) error) error {
	desc := img.Target

	handleManifest := func(ctx context.Context, d ocispec.Descriptor) error {
		platformImg, err := i.NewImageManifest(ctx, img, d)
		if err != nil {
			if errors.Is(err, errNotManifest) {
				return nil
			}
			return err
		}
		return handler(platformImg)
	}

	if c8dimages.IsManifestType(desc.MediaType) {
		return handleManifest(ctx, desc)
	}

	if c8dimages.IsIndexType(desc.MediaType) {
		return c8dimages.Walk(ctx, c8dimages.HandlerFunc(
			func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
				err := handleManifest(ctx, desc)
				if err != nil {
					return nil, err
				}

				descs, err := c8dimages.Children(ctx, i.content, desc)
				if err != nil {
					if cerrdefs.IsNotFound(err) {
						return nil, nil
					}
					return nil, err
				}
				return descs, nil
			}), desc)
	}

	return errNotManifestOrIndex
}

// ImageManifest implements the containerd.Image interface, but all operations
// act on the specific manifest instead of the index as opposed to the struct
// returned by containerd.NewImageWithPlatform.
type ImageManifest struct {
	containerd.Image

	// Parent of the manifest (index/manifest list)
	RealTarget ocispec.Descriptor

	manifest *ocispec.Manifest
}

func (i *ImageService) NewImageManifest(ctx context.Context, img c8dimages.Image, manifestDesc ocispec.Descriptor) (*ImageManifest, error) {
	if !c8dimages.IsManifestType(manifestDesc.MediaType) {
		return nil, errNotManifest
	}

	parent := img.Target
	img.Target = manifestDesc

	c8dImg := containerd.NewImageWithPlatform(i.client, img, platforms.All)
	return &ImageManifest{
		Image:      c8dImg,
		RealTarget: parent,
	}, nil
}

func (im *ImageManifest) Metadata() c8dimages.Image {
	md := im.Image.Metadata()
	md.Target = im.RealTarget
	return md
}

func (im *ImageManifest) IsAttestation() bool {
	// Quick check for buildkit attestation manifests
	// https://github.com/moby/buildkit/blob/v0.11.4/docs/attestations/attestation-storage.md
	// This would have also been caught by the layer check below, but it requires
	// an additional content read and deserialization of Manifest.
	if _, has := im.Target().Annotations[attestation.DockerAnnotationReferenceType]; has {
		return true
	}
	return false
}

// IsPseudoImage returns false when any of the below is true:
// - The manifest has no layers
// - None of its layers is a known image layer.
// - The manifest has unknown/unknown platform.
//
// Some manifests use the image media type for compatibility, even if they are not a real image.
func (im *ImageManifest) IsPseudoImage(ctx context.Context) (bool, error) {
	if im.IsAttestation() {
		return true, nil
	}

	plat := im.Target().Platform
	if plat != nil {
		if plat.OS == "unknown" && plat.Architecture == "unknown" {
			return true, nil
		}
	}

	mfst, err := im.Manifest(ctx)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, errdefs.NotFound(errors.Wrapf(err, "failed to read manifest %v", im.Target().Digest))
		}
		return true, err
	}
	if len(mfst.Layers) == 0 {
		return false, nil
	}
	for _, l := range mfst.Layers {
		if c8dimages.IsLayerType(l.MediaType) {
			return false, nil
		}
	}
	return true, nil
}

func (im *ImageManifest) Manifest(ctx context.Context) (ocispec.Manifest, error) {
	if im.manifest != nil {
		return *im.manifest, nil
	}

	mfst, err := readManifest(ctx, im.ContentStore(), im.Target())
	if err != nil {
		return ocispec.Manifest{}, err
	}

	im.manifest = &mfst
	return mfst, nil
}

func (im *ImageManifest) CheckContentAvailable(ctx context.Context) (bool, error) {
	// The target is already a platform-specific manifest, so no need to match platform.
	pm := platforms.All

	available, _, _, missing, err := c8dimages.Check(ctx, im.ContentStore(), im.Target(), pm)
	if err != nil {
		return false, err
	}

	if !available || len(missing) > 0 {
		return false, nil
	}

	return true, nil
}

func readManifest(ctx context.Context, store content.Provider, desc ocispec.Descriptor) (ocispec.Manifest, error) {
	p, err := content.ReadBlob(ctx, store, desc)
	if err != nil {
		return ocispec.Manifest{}, err
	}

	var mfst ocispec.Manifest
	if err := json.Unmarshal(p, &mfst); err != nil {
		return ocispec.Manifest{}, err
	}

	return mfst, nil
}

// ImagePlatform returns the platform of the image manifest.
// If the manifest list doesn't have a platform filled, it will be read from the config.
func (im *ImageManifest) ImagePlatform(ctx context.Context) (ocispec.Platform, error) {
	target := im.Target()
	if target.Platform != nil {
		return *target.Platform, nil
	}

	var out ocispec.Platform
	err := im.ReadConfig(ctx, &out)
	return out, err
}

// ReadConfig gets the image config and unmarshals it into the provided struct.
// The provided struct should be a pointer to the config struct or its subset.
func (im *ImageManifest) ReadConfig(ctx context.Context, outConfig interface{}) error {
	configDesc, err := im.Config(ctx)
	if err != nil {
		return err
	}

	return readJSON(ctx, im.ContentStore(), configDesc, outConfig)
}

// PresentContentSize returns the size of the image's content that is present in the content store.
func (im *ImageManifest) PresentContentSize(ctx context.Context) (int64, error) {
	cs := im.ContentStore()
	var size int64
	err := c8dimages.Walk(ctx, presentChildrenHandler(cs, func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		size += desc.Size
		return nil, nil
	}), im.Target())
	return size, err
}

// SnapshotUsage returns the disk usage of the image's snapshots.
func (im *ImageManifest) SnapshotUsage(ctx context.Context, snapshotter snapshots.Snapshotter) (snapshots.Usage, error) {
	diffIDs, err := im.RootFS(ctx)
	if err != nil {
		return snapshots.Usage{}, errors.Wrapf(err, "failed to get rootfs of image %s", im.Name())
	}

	imageSnapshotID := identity.ChainID(diffIDs).String()
	unpackedUsage, err := calculateSnapshotTotalUsage(ctx, snapshotter, imageSnapshotID)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return snapshots.Usage{Size: 0}, nil
		}
		log.G(ctx).WithError(err).WithFields(log.Fields{
			"image":      im.Name(),
			"target":     im.Target(),
			"snapshotID": imageSnapshotID,
		}).Warn("failed to calculate snapshot usage of image")

		return snapshots.Usage{}, errors.Wrapf(err, "failed to calculate snapshot usage of image %s", im.Name())
	}
	return unpackedUsage, nil
}
