package build

import (
	"context"
	"fmt"
	"io"

	"github.com/distribution/reference"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/pkg/errors"
)

// tagImages creates image tags for the imageID.
func tagImages(ctx context.Context, ic ImageComponent, stdout io.Writer, imageID image.ID, repoAndTags []reference.Named) error {
	for _, rt := range repoAndTags {
		if err := ic.TagImage(ctx, imageID, rt); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, "Successfully tagged", reference.FamiliarString(rt))
	}
	return nil
}

// sanitizeRepoAndTags parses the raw "t" parameter received from the client
// to a slice of repoAndTag. It removes duplicates, and validates each name
// to not contain a digest.
func sanitizeRepoAndTags(names []string) (repoAndTags []reference.Named, _ error) {
	uniqNames := map[string]struct{}{}
	for _, repo := range names {
		if repo == "" {
			continue
		}

		ref, err := reference.ParseNormalizedNamed(repo)
		if err != nil {
			return nil, err
		}

		if _, ok := ref.(reference.Digested); ok {
			return nil, errors.New("build tag cannot contain a digest")
		}

		ref = reference.TagNameOnly(ref)
		nameWithTag := ref.String()
		if _, exists := uniqNames[nameWithTag]; !exists {
			uniqNames[nameWithTag] = struct{}{}
			repoAndTags = append(repoAndTags, ref)
		}
	}
	return repoAndTags, nil
}
