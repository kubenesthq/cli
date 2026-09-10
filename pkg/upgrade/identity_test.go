package upgrade

import "testing"

func TestUpgradeIdentitySeparatesOwnershipMigration(t *testing.T) {
	ordinary := Options{Cluster: "prod-1", To: "1.1", Servers: []string{"server-1"}}
	migration := ordinary
	migration.MigrateWorkloadApplications = true

	ordinaryIdentity := ordinary.Identity("1.0")
	migrationIdentity := migration.Identity("1.0")
	if ordinaryIdentity.Fields["migrate workload applications"] != "false" {
		t.Fatalf("ordinary identity = %q, want false", ordinaryIdentity.Fields["migrate workload applications"])
	}
	if migrationIdentity.Fields["migrate workload applications"] != "true" {
		t.Fatalf("migration identity = %q, want true", migrationIdentity.Fields["migrate workload applications"])
	}
}
