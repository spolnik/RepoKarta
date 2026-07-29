// Package identity owns enterprise users, groups, roles, and permissions.
package identity

import (
	"context"
	"errors"
	"strings"
	"time"
)

type Role string

const (
	RoleReader     Role = "reader"
	RoleMaintainer Role = "knowledge-maintainer"
	RoleDeveloper  Role = "developer"
	RoleAdmin      Role = "administrator"
)

type Permission string

const (
	PermissionReadRepositories     Permission = "repositories.read"
	PermissionWriteRepositories    Permission = "repositories.write"
	PermissionGenerateAI           Permission = "ai.generate"
	PermissionManageArtifacts      Permission = "artifacts.manage"
	PermissionExportArtifacts      Permission = "artifacts.export"
	PermissionAcquireRepositories  Permission = "repositories.acquire"
	PermissionManageSecurity       Permission = "security.manage"
	PermissionManageRoles          Permission = "roles.manage"
	PermissionViewAudit            Permission = "audit.read"
	PermissionManageAuditRetention Permission = "audit.retention"
	PermissionDestructiveOwnedData Permission = "owned-data.delete"
)

var (
	ErrDeprovisioned = errors.New("identity is suspended or deprovisioned")
	ErrInvalid       = errors.New("invalid identity resource")
	ErrConflict      = errors.New("identity resource conflicts with an existing stable identifier")
)

// User is a durable application identity. AuthProvider/AuthSubject are bound
// when the user first authenticates if SCIM provisioned it ahead of login.
type User struct {
	ID           string    `json:"id"`
	ExternalID   string    `json:"externalId,omitempty"`
	UserName     string    `json:"userName"`
	DisplayName  string    `json:"displayName,omitempty"`
	Email        string    `json:"email,omitempty"`
	AuthProvider string    `json:"auth_provider,omitempty"`
	AuthSubject  string    `json:"auth_subject,omitempty"`
	Active       bool      `json:"active"`
	Role         Role      `json:"role"`
	SCIMManaged  bool      `json:"scim_managed"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Group struct {
	ID          string    `json:"id"`
	ExternalID  string    `json:"externalId,omitempty"`
	DisplayName string    `json:"displayName"`
	Role        Role      `json:"role"`
	Members     []string  `json:"members,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RoleMapping elevates an exact identity-provider group value.
type RoleMapping struct {
	ID         int64     `json:"id"`
	Provider   string    `json:"provider"`
	GroupValue string    `json:"group"`
	Role       Role      `json:"role"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Claims are verified authentication-provider attributes.
type Claims struct {
	Provider string
	Subject  string
	Email    string
	Name     string
	Groups   []string
}

// Resolution is the immediately effective authorization result.
type Resolution struct {
	User  User
	Role  Role
	Known bool
}

// Store is implemented by the durable SQLite store.
type Store interface {
	ResolveIdentity(context.Context, Claims) (Resolution, error)
	ListUsers(context.Context, int, int) ([]User, int, error)
	User(context.Context, string) (User, error)
	SaveUser(context.Context, User) (User, error)
	DeleteUser(context.Context, string) error
	ListGroups(context.Context, int, int) ([]Group, int, error)
	Group(context.Context, string) (Group, error)
	SaveGroup(context.Context, Group) (Group, error)
	DeleteGroup(context.Context, string) error
	ListRoleMappings(context.Context) ([]RoleMapping, error)
	SetRoleMapping(context.Context, RoleMapping) error
	DeleteRoleMapping(context.Context, int64) error
}

func NormalizeRole(role Role) Role {
	switch Role(strings.ToLower(strings.TrimSpace(string(role)))) {
	case RoleAdmin:
		return RoleAdmin
	case RoleDeveloper:
		return RoleDeveloper
	case RoleMaintainer:
		return RoleMaintainer
	default:
		return RoleReader
	}
}

func ValidRole(role Role) bool {
	role = Role(strings.ToLower(strings.TrimSpace(string(role))))
	return role == RoleReader || role == RoleMaintainer ||
		role == RoleDeveloper || role == RoleAdmin
}

func RoleRank(role Role) int {
	switch NormalizeRole(role) {
	case RoleAdmin:
		return 4
	case RoleDeveloper:
		return 3
	case RoleMaintainer:
		return 2
	default:
		return 1
	}
}

func MaxRole(left, right Role) Role {
	if RoleRank(right) > RoleRank(left) {
		return NormalizeRole(right)
	}
	return NormalizeRole(left)
}

// Allows is the explicit M10 permission matrix.
func Allows(role Role, permission Permission) bool {
	role = NormalizeRole(role)
	switch permission {
	case PermissionReadRepositories, PermissionExportArtifacts:
		return true
	case PermissionWriteRepositories:
		return role == RoleDeveloper || role == RoleAdmin
	case PermissionGenerateAI, PermissionManageArtifacts:
		return role == RoleMaintainer || role == RoleDeveloper || role == RoleAdmin
	case PermissionAcquireRepositories,
		PermissionManageSecurity, PermissionManageRoles, PermissionViewAudit,
		PermissionManageAuditRetention, PermissionDestructiveOwnedData:
		return role == RoleAdmin
	default:
		return false
	}
}

// Permissions returns the stable, explicit capabilities for API discovery.
func Permissions(role Role) []Permission {
	all := []Permission{
		PermissionReadRepositories,
		PermissionWriteRepositories,
		PermissionGenerateAI,
		PermissionManageArtifacts,
		PermissionExportArtifacts,
		PermissionAcquireRepositories,
		PermissionManageSecurity,
		PermissionManageRoles,
		PermissionViewAudit,
		PermissionManageAuditRetention,
		PermissionDestructiveOwnedData,
	}
	var allowed []Permission
	for _, permission := range all {
		if Allows(role, permission) {
			allowed = append(allowed, permission)
		}
	}
	return allowed
}
