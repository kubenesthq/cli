package controlplane

import (
	"fmt"
	"strings"
)

// BackendImage is a container image reference split into the fields the chart's
// values carry.
//
// THE SPLIT MATTERS BECAUSE THE CHART RENDERS THE IMAGE FROM THESE FIELDS, and
// from nothing else (kubenest-helm/kubenest/templates/_helpers.tpl
// "kubenest.image"): `repository@digest` when a digest is set, `repository:tag`
// otherwise. So a reference read off a running Deployment has to come back in
// the same shape it was read in, or the apply that is supposed to leave the
// backend running exactly that image renders a different one.
type BackendImage struct {
	Repository string
	Tag        string
	Digest     string
}

// ParseBackendImage splits a container image reference into the repository, the
// tag and the digest it names.
//
// THE REGISTRY'S PORT IS NOT A TAG. `registry.internal:5000/kubenest/backend:675ff0e`
// carries two colons and only the one after the last slash separates the tag; a
// parser that split on the first colon would pin the repository
// `registry.internal` with the tag `5000/kubenest/backend:675ff0e`, and the
// apply would pull something that does not exist.
//
// A REFERENCE WITH NEITHER TAG NOR DIGEST IS REFUSED rather than defaulted:
// Kubernetes resolves it as :latest, and the stage that reads it exists to hold
// the backend to the image it ALREADY runs, which a floating reference cannot
// do. So is a reference whose digest is not `<algorithm>:<hexadecimal>`: it
// would render an image the kubelet rejects, on the one apply that must not
// fail.
func ParseBackendImage(reference string) (BackendImage, error) {
	image := BackendImage{}
	rest := strings.TrimSpace(reference)
	if rest == "" {
		return image, fmt.Errorf("the image reference is empty")
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		digest, err := parseImageDigest(rest[at+1:])
		if err != nil {
			return image, fmt.Errorf("the image reference %q: %w", reference, err)
		}
		image.Digest = digest
		rest = rest[:at]
	}
	// The tag is the colon after the LAST slash: everything before that slash
	// is the repository, its registry host and the host's port included.
	slash := strings.LastIndex(rest, "/")
	if colon := strings.LastIndex(rest, ":"); colon > slash {
		image.Tag = rest[colon+1:]
		rest = rest[:colon]
		if image.Tag == "" {
			return image, fmt.Errorf("the image reference %q names an empty tag", reference)
		}
	}
	if rest == "" {
		return image, fmt.Errorf("the image reference %q names no repository", reference)
	}
	if image.Tag == "" && image.Digest == "" {
		return image, fmt.Errorf("the image reference %q names neither a tag nor a digest, so nothing would hold a pod to what it runs", reference)
	}
	image.Repository = rest
	return image, nil
}

// parseImageDigest checks a digest and returns it unchanged.
func parseImageDigest(digest string) (string, error) {
	algorithm, encoded, found := strings.Cut(digest, ":")
	if !found || algorithm == "" || encoded == "" {
		return "", fmt.Errorf("the digest %q is not <algorithm>:<value>", digest)
	}
	for _, r := range encoded {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return "", fmt.Errorf("the digest %q is not hexadecimal", digest)
		}
	}
	return digest, nil
}
