package controlplane

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/k3s"
)

// POSTGRES STAYS WHAT IT IS (PLAN 7.8, T7.0 item 8).
//
// 1.2 records the Postgres major version and the exact image the control-plane
// chart runs, pins them by digest, and does NOT change either. A chart whose
// Postgres major or distribution differs from the running one is REFUSED here,
// naming the fact that a migration of either is its own tested procedure in a
// later release — because the alternative is discovering it during the upgrade,
// with the customer's organisations and token floors inside the database.
//
// The comparison is between TWO IMAGES: the one the chart will run and the one
// the StatefulSet is running. It is deliberately symmetric — either side moving
// is the same refusal — so the property can be exercised by moving the running
// side in a test, which is a read-only change to a StatefulSet and needs no
// second chart.
const (
	// postgresStatefulSetImageCmd reads the image the running PostgreSQL
	// StatefulSet uses.
	postgresStatefulSetImageCmd = "get statefulset " + postgresStatefulSet + " -n " + Namespace +
		" -o jsonpath={.spec.template.spec.containers[0].image}"

	// postgresVersionCmd asks the RUNNING SERVER what it is, inside the pod the
	// chart created. The container name is the Bitnami subchart's ("postgresql",
	// kubenest/charts/postgresql/templates/primary/statefulset.yaml) and is
	// named explicitly, because a StatefulSet with any other container would
	// otherwise have this read pick the wrong one.
	postgresVersionCmd = "exec -n " + Namespace + " statefulset/" + postgresStatefulSet +
		" -c postgresql -- postgres --version"
)

// postgresVersionLine matches the version `postgres --version` prints:
// "postgres (PostgreSQL) 17.6" (and "17.6 (Debian ...)" on some builds).
var postgresVersionLine = regexp.MustCompile(`\((?:PostgreSQL|Debian)[^)]*\)\s*(\d+)`)

// postgresImage is one PostgreSQL image reference, split into the two facts
// that must not change.
type postgresImage struct {
	// repository is the full path WITHOUT the tag or digest, e.g.
	// "docker.io/bitnami/postgresql". It is the DISTRIBUTION: the same major
	// from a different builder is a different database with a different
	// upgrade story.
	repository string
	// tag is the tag, e.g. "17.2.0-debian-12-r0".
	tag string
	// digest is the sha256 digest when the reference carries one.
	digest string
	// major is the PostgreSQL major version the tag names.
	major string
}

// tagMajor matches the leading major of a Bitnami PostgreSQL tag, which is the
// PostgreSQL version: "17.2.0-debian-12-r0", "18.1.0".
var tagMajor = regexp.MustCompile(`^(\d+)`)

// distribution is the part of a repository that names WHICH PostgreSQL this is:
// the path inside its registry, with the registry host normalised away.
//
// THE REFERENCE IS NOT COMPARABLE RAW. The chart declares `bitnami/postgresql`
// unqualified — deliberately, because the Bitnami subchart prepends its own
// registry, and qualifying it produced an unpullable
// registry-1.docker.io/docker.io/... (found on real hardware 2026-09-25) —
// while the StatefulSet renders `registry-1.docker.io/bitnami/postgresql`.
// Comparing the two strings would report a change of distribution on every
// upgrade, which is the same gate never passing from the other direction.
func (p postgresImage) distribution() string {
	parts := strings.Split(p.repository, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		parts = parts[1:]
	}
	repo := strings.Join(parts, "/")
	// Docker Hub's own aliases, so "docker.io/library/postgres" and
	// "library/postgres" are the same distribution.
	for _, alias := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		repo = strings.TrimPrefix(repo, alias)
	}
	return repo
}

// parsePostgresImage splits a container image reference.
//
// It handles the forms a StatefulSet actually carries: with a tag, with a
// digest, and with both ("repo:tag@sha256:..."). A reference with neither is
// left with an empty tag, which compares as "unknown" rather than as a
// difference.
func parsePostgresImage(ref string) postgresImage {
	out := postgresImage{}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return out
	}
	body := ref
	if at := strings.LastIndex(body, "@"); at >= 0 {
		out.digest = body[at+1:]
		body = body[:at]
	}
	// The tag separator is the last ':' AFTER the last '/': a registry host
	// carries a port in exactly that position.
	slash := strings.LastIndex(body, "/")
	if colon := strings.LastIndex(body, ":"); colon > slash {
		out.tag = body[colon+1:]
		body = body[:colon]
	}
	out.repository = body
	if m := tagMajor.FindStringSubmatch(out.tag); m != nil {
		out.major = m[1]
	}
	return out
}

// postgresPin is what the chart declares it will run.
type postgresPin struct {
	image postgresImage
}

// ChartPostgresImage reads the PostgreSQL the embedded chart will run.
//
// THE CHART IS THE SOURCE OF THE ANSWER, not a second copy of it: the values
// document the CLI renders carries the database password and nothing about the
// image, so the pin lives in the chart's own values.yaml — inside the archive
// the CLI applies. Reading it from there is what makes the comparison about the
// artifact that is actually going to be installed.
//
// A values override wins when the caller has one, because a document that sets
// postgresql.image is a chart whose PostgreSQL IS that image, whatever the
// archive's own values say.
//
// THE MAJOR IS NOT IN THE IMAGE HERE, and the chart is why: it pins a DIGEST,
// so the running and declared references carry no tag to read a version out of.
// The chart's own declaration of its major is the Bitnami subchart's
// appVersion, which is the PostgreSQL version the subchart was built for — and
// the chart's own comment says two other things already depend on it (the
// checkpoint drill's scratch server and the backend's postgresql-client).
func ChartPostgresImage(valuesYAML string) (postgresImage, error) {
	image, ok, err := postgresImageFromValues(valuesYAML)
	if err != nil {
		return postgresImage{}, err
	}
	if !ok {
		body, err := ChartFile("values.yaml")
		if err != nil {
			return postgresImage{}, fmt.Errorf("reading the control-plane chart's values: %w", err)
		}
		image, ok, err = postgresImageFromValues(string(body))
		if err != nil {
			return postgresImage{}, fmt.Errorf("reading the control-plane chart's values: %w", err)
		}
		if !ok {
			return postgresImage{}, fmt.Errorf("the control-plane chart's values.yaml declares no postgresql.image, so the PostgreSQL this upgrade would run cannot be established; an upgrade must not run a database nobody can name")
		}
	}
	if image.major == "" {
		major, err := subchartPostgresMajor()
		if err != nil {
			return postgresImage{}, err
		}
		image.major = major
	}
	return image, nil
}

// subchartPostgresMajor reads the PostgreSQL major the chart's Bitnami
// subchart declares, from the subchart's appVersion inside the archive.
func subchartPostgresMajor() (string, error) {
	body, err := ChartFile("charts/postgresql/Chart.yaml")
	if err != nil {
		return "", fmt.Errorf("reading the chart's PostgreSQL subchart metadata: %w", err)
	}
	var meta struct {
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(body, &meta); err != nil {
		return "", fmt.Errorf("the PostgreSQL subchart's Chart.yaml is unparsable: %w", err)
	}
	major := tagMajor.FindString(meta.AppVersion)
	if major == "" {
		return "", fmt.Errorf("the chart's PostgreSQL subchart declares appVersion %q, which does not name a major version, so the PostgreSQL this upgrade would run cannot be established", meta.AppVersion)
	}
	return major, nil
}

// postgresImageFromValues extracts postgresql.image from a values document.
func postgresImageFromValues(valuesYAML string) (postgresImage, bool, error) {
	if strings.TrimSpace(valuesYAML) == "" {
		return postgresImage{}, false, nil
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesYAML), &doc); err != nil {
		return postgresImage{}, false, err
	}
	postgres, _ := doc["postgresql"].(map[string]any)
	if postgres == nil {
		return postgresImage{}, false, nil
	}
	image, _ := postgres["image"].(map[string]any)
	if image == nil {
		return postgresImage{}, false, nil
	}
	repository, _ := image["repository"].(string)
	tag, _ := image["tag"].(string)
	digest, _ := image["digest"].(string)
	if repository == "" && tag == "" && digest == "" {
		return postgresImage{}, false, nil
	}
	ref := repository
	if tag != "" {
		ref += ":" + tag
	}
	if digest != "" {
		ref += "@" + digest
	}
	return parsePostgresImage(ref), true, nil
}

// RunningPostgresImage reads the PostgreSQL the control plane runs now.
//
// THE MAJOR COMES FROM THE RUNNING SERVER BEFORE ANY FALLBACK, and the order
// below is the fix for a refusal that hit every FIRST upgrade: this chart pins
// PostgreSQL by DIGEST, so the StatefulSet's reference carries no tag, and the
// eligible checkpoint does not exist before the first upgrade because the
// checkpoint stage runs after the gates (observed on a fresh host, 2026-09-25).
//
//	tag         when the reference carries one, which is free and exact;
//	the SERVER  `postgres --version` in the running pod: the authoritative
//	            answer, and the only one that is right for a database whose
//	            image is pinned by digest;
//	checkpoint  the eligible checkpoint's `postgres_major`, which the runner
//	            derives from the live server's own `server_version_num`.
//
// Still FAILS CLOSED when none of the three answers, because "I could not tell"
// is not "they are the same" for the gate that stands between an upgrade and
// the installation's database.
func RunningPostgresImage(ctx context.Context, r k3s.Runner) (postgresImage, error) {
	out, err := k3s.Kubectl(ctx, r, postgresStatefulSetImageCmd)
	if err != nil {
		return postgresImage{}, fmt.Errorf("reading the running PostgreSQL StatefulSet %s/%s: %w", Namespace, postgresStatefulSet, err)
	}
	ref := strings.TrimSpace(out)
	if ref == "" {
		return postgresImage{}, fmt.Errorf("the PostgreSQL StatefulSet %s/%s names no image, so what this control plane runs cannot be established", Namespace, postgresStatefulSet)
	}
	image := parsePostgresImage(ref)
	if image.major != "" {
		return image, nil
	}
	if major := runningPostgresMajorFromServer(ctx, r); major != "" {
		image.major = major
		return image, nil
	}
	checkpoint, err := ReadEligibleCheckpoint(ctx, r)
	if err != nil {
		return postgresImage{}, err
	}
	if checkpoint == nil || checkpoint.PostgresMajor == 0 {
		return postgresImage{}, fmt.Errorf("the running PostgreSQL image %s is pinned by digest, the running server did not report its version (%s), and no eligible checkpoint records it, so the database this control plane runs cannot be established — and an upgrade must not proceed without knowing whether it would change the database", ref, postgresVersionCmd)
	}
	image.major = fmt.Sprintf("%d", checkpoint.PostgresMajor)
	return image, nil
}

// runningPostgresMajorFromServer asks the running server what it is.
//
// A read that cannot answer is NOT an error here: it is the reason the caller
// has two more sources to try, and the last of them refuses. The failure is
// reported by whatever the caller does when every source is silent.
func runningPostgresMajorFromServer(ctx context.Context, r k3s.Runner) string {
	out, err := k3s.Kubectl(ctx, r, postgresVersionCmd)
	if err != nil {
		return ""
	}
	match := postgresVersionLine.FindStringSubmatch(out)
	if match == nil {
		return ""
	}
	return match[1]
}

// PostgresRefusal is the difference between the two images.
type PostgresRefusal struct {
	Running   string
	Declared  string
	Reason    string
	RunningPG postgresImage
	ChartPG   postgresImage
}

func (r *PostgresRefusal) Error() string {
	return fmt.Sprintf(
		"this upgrade would change the control plane's PostgreSQL, so it is refused.\n"+
			"      Why: %s. The database holds every organisation, member, role, window, alert route, inventory and token floor of this installation.\n"+
			"      Running: %s\n"+
			"      Chart:   %s\n"+
			"      Fix: 1.2 does not change the PostgreSQL major or distribution. Pin the chart to the running database (%s), or run the PostgreSQL migration procedure this release does not carry — it is its own tested procedure, in a later release, not a side effect of an upgrade",
		r.Reason, r.Running, r.Declared, r.RunningPG.repository+":"+r.RunningPG.tag)
}

// CheckPostgresUnchanged refuses an upgrade whose chart would change the
// PostgreSQL major version or distribution.
func CheckPostgresUnchanged(ctx context.Context, r k3s.Runner, valuesYAML string) error {
	declared, err := ChartPostgresImage(valuesYAML)
	if err != nil {
		return err
	}
	running, err := RunningPostgresImage(ctx, r)
	if err != nil {
		return err
	}
	return comparePostgres(running, declared)
}

// comparePostgres is the comparison on its own, so the refusal's terms are
// testable without a cluster.
func comparePostgres(running, declared postgresImage) error {
	// The DISTRIBUTION first: the same major from a different builder is a
	// different database, and a tag would not tell anyone that.
	if running.distribution() != declared.distribution() {
		return &PostgresRefusal{
			Running: running.repository, Declared: declared.repository,
			Reason: fmt.Sprintf("the chart's PostgreSQL comes from %s and the running one from %s, so this is a change of distribution rather than of version",
				declared.repository, running.repository),
			RunningPG: running, ChartPG: declared,
		}
	}
	switch {
	case running.major == "" || declared.major == "":
		// An image whose tag does not begin with its major is one this build
		// cannot compare. It is REFUSED rather than passed: "I could not tell"
		// is not "they are the same", and this is the check that stands between
		// an upgrade and the installation's database.
		return &PostgresRefusal{
			Running: running.repository + ":" + running.tag, Declared: declared.repository + ":" + declared.tag,
			Reason:    "one of the two PostgreSQL images does not name its major version in a form this build can read",
			RunningPG: running, ChartPG: declared,
		}
	case running.major != declared.major:
		return &PostgresRefusal{
			Running: running.repository + ":" + running.tag, Declared: declared.repository + ":" + declared.tag,
			Reason: fmt.Sprintf("the chart would run PostgreSQL %s where %s is running, and a major-version migration is not part of this upgrade",
				declared.major, running.major),
			RunningPG: running, ChartPG: declared,
		}
	}
	return nil
}

// postgresImageName is the human name of an image reference, tag and digest.
func postgresImageName(image postgresImage) string {
	name := path.Join(image.repository)
	if image.tag != "" {
		name += ":" + image.tag
	}
	if image.digest != "" {
		name += "@" + image.digest
	}
	return name
}
