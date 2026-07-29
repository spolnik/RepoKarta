package identity

import (
	"reflect"
	"testing"
)

func TestRolesPermissionsAndRanking(t *testing.T) {
	if NormalizeRole(" ADMINISTRATOR ") != RoleAdmin ||
		NormalizeRole(" developer ") != RoleDeveloper ||
		NormalizeRole("knowledge-maintainer") != RoleMaintainer ||
		NormalizeRole("unknown") != RoleReader {
		t.Fatal("role normalization changed")
	}
	if !ValidRole(RoleReader) || !ValidRole(RoleMaintainer) ||
		!ValidRole(RoleDeveloper) || !ValidRole(RoleAdmin) || ValidRole("owner") {
		t.Fatal("role validation changed")
	}
	if RoleRank(RoleAdmin) != 4 || RoleRank(RoleDeveloper) != 3 ||
		RoleRank(RoleMaintainer) != 2 || RoleRank(RoleReader) != 1 {
		t.Fatal("role ranking changed")
	}
	if MaxRole(RoleReader, RoleDeveloper) != RoleDeveloper ||
		MaxRole(RoleMaintainer, RoleReader) != RoleMaintainer {
		t.Fatal("maximum role changed")
	}
	if Allows(RoleReader, PermissionGenerateAI) ||
		Allows(RoleMaintainer, PermissionWriteRepositories) ||
		!Allows(RoleDeveloper, PermissionWriteRepositories) ||
		!Allows(RoleAdmin, PermissionWriteRepositories) ||
		!Allows(RoleMaintainer, PermissionGenerateAI) ||
		!Allows(RoleAdmin, PermissionManageSecurity) ||
		Allows(RoleAdmin, Permission("unknown")) {
		t.Fatal("permission matrix changed")
	}
	if got := Permissions(RoleReader); !reflect.DeepEqual(got, []Permission{
		PermissionReadRepositories, PermissionExportArtifacts,
	}) {
		t.Fatalf("reader permissions = %#v", got)
	}
	if len(Permissions(RoleAdmin)) != 11 {
		t.Fatalf("administrator permissions = %#v", Permissions(RoleAdmin))
	}
}
