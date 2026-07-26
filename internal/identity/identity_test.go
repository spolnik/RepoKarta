package identity

import "testing"

func TestPermissionMatrix(t *testing.T) {
	tests := []struct {
		role       Role
		permission Permission
		want       bool
	}{
		{RoleReader, PermissionReadRepositories, true},
		{RoleReader, PermissionExportArtifacts, true},
		{RoleReader, PermissionGenerateAI, false},
		{RoleMaintainer, PermissionGenerateAI, true},
		{RoleMaintainer, PermissionManageArtifacts, true},
		{RoleMaintainer, PermissionViewCrossAuthor, false},
		{RoleAdmin, PermissionViewCrossAuthor, true},
		{RoleAdmin, PermissionAcquireRepositories, true},
		{RoleAdmin, PermissionManageSecurity, true},
		{RoleAdmin, PermissionManageRoles, true},
		{RoleAdmin, PermissionViewAudit, true},
	}
	for _, test := range tests {
		if got := Allows(test.role, test.permission); got != test.want {
			t.Errorf("Allows(%q, %q) = %v, want %v", test.role, test.permission, got, test.want)
		}
	}
}

func TestUnknownRoleNeverElevates(t *testing.T) {
	if NormalizeRole("owner") != RoleReader {
		t.Fatal("unknown role did not fail closed to reader")
	}
	if Allows("owner", PermissionManageRoles) {
		t.Fatal("unknown role received elevated permission")
	}
}
