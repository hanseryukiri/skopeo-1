package buildah

import (
	"bytes"
	"io"
	"time"

	"github.com/Sirupsen/logrus"
	cp "github.com/containers/image/copy"
	"github.com/containers/image/signature"
	is "github.com/containers/image/storage"
	"github.com/containers/image/transports"
	"github.com/containers/image/types"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/pkg/stringid"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/projectatomic/buildah/util"
)

var (
	// gzippedEmptyLayer is a gzip-compressed version of an empty tar file (just 1024 zero bytes).  This
	// comes from github.com/docker/distribution/manifest/schema1/config_builder.go by way of
	// github.com/containers/image/image/docker_schema2.go; there is a non-zero embedded timestamp; we could
	// zero that, but that would just waste storage space in registries, so let’s use the same values.
	gzippedEmptyLayer = []byte{
		31, 139, 8, 0, 0, 9, 110, 136, 0, 255, 98, 24, 5, 163, 96, 20, 140, 88,
		0, 8, 0, 0, 255, 255, 46, 175, 181, 239, 0, 4, 0, 0,
	}
)

// CommitOptions can be used to alter how an image is committed.
type CommitOptions struct {
	// PreferredManifestType is the preferred type of image manifest.  The
	// image configuration format will be of a compatible type.
	PreferredManifestType string
	// Compression specifies the type of compression which is applied to
	// layer blobs.  The default is to not use compression, but
	// archive.Gzip is recommended.
	Compression archive.Compression
	// SignaturePolicyPath specifies an override location for the signature
	// policy which should be used for verifying the new image as it is
	// being written.  Except in specific circumstances, no value should be
	// specified, indicating that the shared, system-wide default policy
	// should be used.
	SignaturePolicyPath string
	// AdditionalTags is a list of additional names to add to the image, if
	// the transport to which we're writing the image gives us a way to add
	// them.
	AdditionalTags []string
	// ReportWriter is an io.Writer which will be used to log the writing
	// of the new image.
	ReportWriter io.Writer
	// HistoryTimestamp is the timestamp used when creating new items in the
	// image's history.  If unset, the current time will be used.
	HistoryTimestamp *time.Time
}

// PushOptions can be used to alter how an image is copied somewhere.
type PushOptions struct {
	// Compression specifies the type of compression which is applied to
	// layer blobs.  The default is to not use compression, but
	// archive.Gzip is recommended.
	Compression archive.Compression
	// SignaturePolicyPath specifies an override location for the signature
	// policy which should be used for verifying the new image as it is
	// being written.  Except in specific circumstances, no value should be
	// specified, indicating that the shared, system-wide default policy
	// should be used.
	SignaturePolicyPath string
	// ReportWriter is an io.Writer which will be used to log the writing
	// of the new image.
	ReportWriter io.Writer
	// Store is the local storage store which holds the source image.
	Store storage.Store
}

// shallowCopy copies the most recent layer, the configuration, and the manifest from one image to another.
// For local storage, which doesn't care about histories and the manifest's contents, that's sufficient, but
// almost any other destination has higher expectations.
// We assume that "dest" is a reference to a local image (specifically, a containers/image/storage.storageReference),
// and will fail if it isn't.
func (b *Builder) shallowCopy(dest types.ImageReference, src types.ImageReference, systemContext *types.SystemContext) error {
	// Read the target image name.
	if dest.DockerReference() == nil {
		return errors.New("can't write to an unnamed image")
	}
	names, err := util.ExpandTags([]string{dest.DockerReference().String()})
	if err != nil {
		return err
	}
	// Make a temporary image reference.
	tmpName := stringid.GenerateRandomID() + "-tmp-" + Package + "-commit"
	tmpRef, err := is.Transport.ParseStoreReference(b.store, tmpName)
	if err != nil {
		return err
	}
	defer func() {
		if err2 := tmpRef.DeleteImage(systemContext); err2 != nil {
			logrus.Debugf("error deleting temporary image %q: %v", tmpName, err2)
		}
	}()
	// Open the source for reading and a temporary image for writing.
	srcImage, err := src.NewImage(systemContext)
	if err != nil {
		return errors.Wrapf(err, "error reading configuration to write to image %q", transports.ImageName(dest))
	}
	defer srcImage.Close()
	tmpImage, err := tmpRef.NewImageDestination(systemContext)
	if err != nil {
		return errors.Wrapf(err, "error opening temporary copy of image %q for writing", transports.ImageName(dest))
	}
	defer tmpImage.Close()
	// Write an empty filesystem layer, because the image layer requires at least one.
	_, err = tmpImage.PutBlob(bytes.NewReader(gzippedEmptyLayer), types.BlobInfo{Size: int64(len(gzippedEmptyLayer))})
	if err != nil {
		return errors.Wrapf(err, "error writing dummy layer for image %q", transports.ImageName(dest))
	}
	// Read the newly-generated configuration blob.
	config, err := srcImage.ConfigBlob()
	if err != nil {
		return errors.Wrapf(err, "error reading new configuration for image %q", transports.ImageName(dest))
	}
	if len(config) == 0 {
		return errors.Errorf("error reading new configuration for image %q: it's empty", transports.ImageName(dest))
	}
	logrus.Debugf("read configuration blob %q", string(config))
	// Write the configuration to the temporary image.
	configBlobInfo := types.BlobInfo{
		Digest: digest.Canonical.FromBytes(config),
		Size:   int64(len(config)),
	}
	_, err = tmpImage.PutBlob(bytes.NewReader(config), configBlobInfo)
	if err != nil && len(config) > 0 {
		return errors.Wrapf(err, "error writing image configuration for temporary copy of %q", transports.ImageName(dest))
	}
	// Read the newly-generated, mostly fake, manifest.
	manifest, _, err := srcImage.Manifest()
	if err != nil {
		return errors.Wrapf(err, "error reading new manifest for image %q", transports.ImageName(dest))
	}
	// Write the manifest to the temporary image.
	err = tmpImage.PutManifest(manifest)
	if err != nil {
		return errors.Wrapf(err, "error writing new manifest to temporary copy of image %q", transports.ImageName(dest))
	}
	// Save the temporary image.
	err = tmpImage.Commit()
	if err != nil {
		return errors.Wrapf(err, "error committing new image %q", transports.ImageName(dest))
	}
	// Locate the temporary image in the lower-level API.  Read its item names.
	tmpImg, err := is.Transport.GetStoreImage(b.store, tmpRef)
	if err != nil {
		return errors.Wrapf(err, "error locating temporary image %q", transports.ImageName(dest))
	}
	items, err := b.store.ListImageBigData(tmpImg.ID)
	if err != nil {
		return errors.Wrapf(err, "error reading list of named data for image %q", tmpImg.ID)
	}
	// Look up the container's read-write layer.
	container, err := b.store.Container(b.ContainerID)
	if err != nil {
		return errors.Wrapf(err, "error reading information about working container %q", b.ContainerID)
	}
	parentLayer := ""
	// Look up the container's source image's layer, if there is a source image.
	if container.ImageID != "" {
		img, err2 := b.store.Image(container.ImageID)
		if err2 != nil {
			return errors.Wrapf(err2, "error reading information about working container %q's source image", b.ContainerID)
		}
		parentLayer = img.TopLayer
	}
	// Extract the read-write layer's contents.
	layerDiff, err := b.store.Diff(parentLayer, container.LayerID)
	if err != nil {
		return errors.Wrapf(err, "error reading layer from source image %q", transports.ImageName(src))
	}
	defer layerDiff.Close()
	// Write a copy of the layer for the new image to reference.
	layer, _, err := b.store.PutLayer("", parentLayer, []string{}, "", false, layerDiff)
	if err != nil {
		return errors.Wrapf(err, "error creating new read-only layer from container %q", b.ContainerID)
	}
	// Create a low-level image record that uses the new layer.
	image, err := b.store.CreateImage("", []string{}, layer.ID, "", nil)
	if err != nil {
		err2 := b.store.DeleteLayer(layer.ID)
		if err2 != nil {
			logrus.Debugf("error removing layer %q: %v", layer, err2)
		}
		return errors.Wrapf(err, "error creating new low-level image %q", transports.ImageName(dest))
	}
	logrus.Debugf("created image ID %q", image.ID)
	defer func() {
		if err != nil {
			_, err2 := b.store.DeleteImage(image.ID, true)
			if err2 != nil {
				logrus.Debugf("error removing image %q: %v", image.ID, err2)
			}
		}
	}()
	// Copy the configuration and manifest, which are big data items, along with whatever else is there.
	for _, item := range items {
		var data []byte
		data, err = b.store.ImageBigData(tmpImg.ID, item)
		if err != nil {
			return errors.Wrapf(err, "error copying data item %q", item)
		}
		err = b.store.SetImageBigData(image.ID, item, data)
		if err != nil {
			return errors.Wrapf(err, "error copying data item %q", item)
		}
		logrus.Debugf("copied data item %q to %q", item, image.ID)
	}
	// Set low-level metadata in the new image so that the image library will accept it as a real image.
	err = b.store.SetMetadata(image.ID, "{}")
	if err != nil {
		return errors.Wrapf(err, "error assigning metadata to new image %q", transports.ImageName(dest))
	}
	// Move the target name(s) from the temporary image to the new image.
	err = util.AddImageNames(b.store, image, names)
	if err != nil {
		return errors.Wrapf(err, "error assigning names %v to new image", names)
	}
	logrus.Debugf("assigned names %v to image %q", names, image.ID)
	return nil
}

// Commit writes the contents of the container, along with its updated
// configuration, to a new image in the specified location, and if we know how,
// add any additional tags that were specified.
func (b *Builder) Commit(dest types.ImageReference, options CommitOptions) error {
	policy, err := signature.DefaultPolicy(getSystemContext(options.SignaturePolicyPath))
	if err != nil {
		return err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return err
	}
	// Check if we're keeping everything in local storage.  If so, we can take certain shortcuts.
	_, destIsStorage := dest.Transport().(is.StoreTransport)
	exporting := !destIsStorage
	src, err := b.makeContainerImageRef(options.PreferredManifestType, exporting, options.Compression, options.HistoryTimestamp)
	if err != nil {
		return errors.Wrapf(err, "error computing layer digests and building metadata")
	}
	if exporting {
		// Copy everything.
		err = cp.Image(policyContext, dest, src, getCopyOptions(options.ReportWriter))
		if err != nil {
			return errors.Wrapf(err, "error copying layers and metadata")
		}
	} else {
		// Copy only the most recent layer, the configuration, and the manifest.
		err = b.shallowCopy(dest, src, getSystemContext(options.SignaturePolicyPath))
		if err != nil {
			return errors.Wrapf(err, "error copying layer and metadata")
		}
	}
	if len(options.AdditionalTags) > 0 {
		switch dest.Transport().Name() {
		case is.Transport.Name():
			img, err := is.Transport.GetStoreImage(b.store, dest)
			if err != nil {
				return errors.Wrapf(err, "error locating just-written image %q", transports.ImageName(dest))
			}
			err = util.AddImageNames(b.store, img, options.AdditionalTags)
			if err != nil {
				return errors.Wrapf(err, "error setting image names to %v", append(img.Names, options.AdditionalTags...))
			}
			logrus.Debugf("assigned names %v to image %q", img.Names, img.ID)
		default:
			logrus.Warnf("don't know how to add tags to images stored in %q transport", dest.Transport().Name())
		}
	}
	return nil
}

// Push copies the contents of the image to a new location.
func Push(image string, dest types.ImageReference, options PushOptions) error {
	systemContext := getSystemContext(options.SignaturePolicyPath)
	policy, err := signature.DefaultPolicy(systemContext)
	if err != nil {
		return err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return err
	}
	importOptions := ImportFromImageOptions{
		Image:               image,
		SignaturePolicyPath: options.SignaturePolicyPath,
	}
	builder, err := importBuilderFromImage(options.Store, importOptions)
	if err != nil {
		return errors.Wrap(err, "error importing builder information from image")
	}
	// Look up the image name and its layer.
	ref, err := is.Transport.ParseStoreReference(options.Store, image)
	if err != nil {
		return errors.Wrapf(err, "error parsing reference to image %q", image)
	}
	img, err := is.Transport.GetStoreImage(options.Store, ref)
	if err != nil {
		return errors.Wrapf(err, "error locating image %q", image)
	}
	// Give the image we're producing the same ancestors as its source image.
	builder.FromImage = builder.Docker.ContainerConfig.Image
	builder.FromImageID = string(builder.Docker.Parent)
	// Prep the layers and manifest for export.
	src, err := builder.makeImageImageRef(options.Compression, img.Names, img.TopLayer, nil)
	if err != nil {
		return errors.Wrapf(err, "error recomputing layer digests and building metadata")
	}
	// Copy everything.
	err = cp.Image(policyContext, dest, src, getCopyOptions(options.ReportWriter))
	if err != nil {
		return errors.Wrapf(err, "error copying layers and metadata")
	}
	return nil
}