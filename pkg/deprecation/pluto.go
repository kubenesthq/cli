package deprecation

import (
	"context"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/manifest"
)

// installDir is where the pinned scanner lives on the server node. It is
// versioned in the path so a bundle bump installs a new binary rather than
// silently reusing an old one — the version in the path IS the check that the
// dataset matches the pin.
const installDir = "/var/lib/rancher/kubenest/bin"

// ReleaseBaseURL is pluto's release download base. A variable so tests can
// point it at a local server.
var ReleaseBaseURL = "https://github.com/FairwindsOps/pluto/releases/download"

// ensurePluto makes the pinned scanner present on the node and returns its
// path.
//
// The binary is fetched to a version-stamped path, so the pin and what runs
// cannot drift: a manifest that moves the pin gets a different path and
// therefore a different download. There is no "latest", and an already
// present binary at the pinned path is reused rather than re-fetched, which
// makes a resumed upgrade cheap.
func ensurePluto(ctx context.Context, r Runner, scanner manifest.DeprecationScanner) (string, error) {
	path := fmt.Sprintf("%s/pluto-%s", installDir, scanner.Version)

	present, err := run(ctx, r, fmt.Sprintf("test -x %s && echo present || echo absent", shellQuote(path)))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(present) == "present" {
		return path, nil
	}

	arch, err := run(ctx, r, "uname -m")
	if err != nil {
		return "", err
	}
	goarch, err := archOf(strings.TrimSpace(arch))
	if err != nil {
		return "", err
	}

	// pluto's release assets are pluto_<version-without-v>_linux_<arch>.tar.gz.
	version := strings.TrimPrefix(scanner.Version, "v")
	url := fmt.Sprintf("%s/%s/pluto_%s_linux_%s.tar.gz", ReleaseBaseURL, scanner.Version, version, goarch)

	script := strings.Join([]string{
		fmt.Sprintf("sudo -n install -d -m 0755 %s", installDir),
		"tmp=$(mktemp -d)",
		fmt.Sprintf("curl -sfL %s -o \"$tmp/pluto.tar.gz\"", shellQuote(url)),
		"tar -xzf \"$tmp/pluto.tar.gz\" -C \"$tmp\" pluto",
		fmt.Sprintf("sudo -n install -m 0755 \"$tmp/pluto\" %s", shellQuote(path)),
		"rm -rf \"$tmp\"",
	}, " && ")

	if _, err := run(ctx, r, script); err != nil {
		return "", fmt.Errorf("installing the pinned scanner %s %s from %s: %w", scanner.Tool, scanner.Version, url, err)
	}

	// Prove what landed is what was pinned, rather than assuming the
	// download was what the URL claimed.
	reported, err := run(ctx, r, shellQuote(path)+" version 2>&1 | head -1")
	if err != nil {
		return "", err
	}
	if !strings.Contains(reported, version) {
		return "", fmt.Errorf("the scanner on the node reports %q but the bundle pins %s: the dataset behind a scan must be the pinned one, so this is refused rather than trusted",
			strings.TrimSpace(reported), scanner.Version)
	}
	return path, nil
}

// archOf maps uname -m to the release asset's architecture. An architecture
// with no pinned asset is an error rather than a guess: downloading the wrong
// binary would fail later and less clearly.
func archOf(uname string) (string, error) {
	switch uname {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("no pinned scanner build for architecture %q", uname)
	}
}
