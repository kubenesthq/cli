package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// ControlPlaneVersion is the control plane's own identity, as GET
// /api/v1/version answers it (kn-control-plane-version-identity-frev).
//
// TWO FIELDS, TWO QUESTIONS. Contract is the MONOTONIC integer era this build
// implements, and it is the only orderable one: comparing two of them answers
// "is this control plane at or after the point where behaviour X changed"
// without a repository. Build is the git SHA of the image, which says which
// commit is running and cannot be ordered by anyone but the repository's owner.
type ControlPlaneVersion struct {
	Contract int    `json:"contract"`
	Build    string `json:"build"`
}

// ErrVersionEndpointAbsent is returned when the control plane does not serve
// GET /api/v1/version at all.
//
// A 404 IS "CANNOT TELL", NOT "DOES NOT HAVE IT". The route begins with the
// contract counter, so every control plane built before it answers 404 —
// including ones that already carry behaviour a caller is asking about. A
// caller must therefore treat this as "no answer", never as a missing
// capability, and the sentinel exists so that distinction cannot be lost by a
// caller matching on an HTTP status it did not expect.
var ErrVersionEndpointAbsent = errors.New("this control plane does not serve GET /api/v1/version")

// IsVersionEndpointAbsent reports whether err is the control plane declining
// to answer the version question at all, rather than failing to answer it.
func IsVersionEndpointAbsent(err error) bool {
	return errors.Is(err, ErrVersionEndpointAbsent)
}

// ControlPlaneVersion reads the control plane's own contract era and build
// stamp. It is the only call in this package whose 404 carries meaning: see
// ErrVersionEndpointAbsent.
func (c *Client) ControlPlaneVersion(ctx context.Context) (ControlPlaneVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/api/v1/version"), nil)
	if err != nil {
		return ControlPlaneVersion{}, err
	}
	req.Header.Set("Accept", "application/json")

	var out ControlPlaneVersion
	if err := c.do(req, &out); err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return ControlPlaneVersion{}, fmt.Errorf("%w: %w", ErrVersionEndpointAbsent, err)
		}
		return ControlPlaneVersion{}, err
	}
	return out, nil
}
