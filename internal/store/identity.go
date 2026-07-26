package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/identity"
)

// ResolveIdentity creates an observed reader or resolves a provisioned user,
// then evaluates direct, SCIM-group, and provider-group roles on every request.
func (s *Store) ResolveIdentity(ctx context.Context, claims identity.Claims) (identity.Resolution, error) {
	claims.Provider = strings.TrimSpace(claims.Provider)
	claims.Subject = strings.TrimSpace(claims.Subject)
	claims.Email = strings.TrimSpace(claims.Email)
	claims.Name = strings.TrimSpace(claims.Name)
	if claims.Provider == "" || claims.Subject == "" {
		return identity.Resolution{}, errors.New("authentication provider and stable subject are required")
	}

	user, err := s.identityByClaims(ctx, claims)
	known := err == nil
	if errors.Is(err, sql.ErrNoRows) {
		user = identity.User{
			ID:           newStoreID("usr"),
			UserName:     firstIdentityValue(claims.Email, claims.Provider+":"+claims.Subject),
			DisplayName:  claims.Name,
			Email:        claims.Email,
			AuthProvider: claims.Provider,
			AuthSubject:  claims.Subject,
			Active:       true,
			Role:         identity.RoleReader,
		}
		user, err = s.SaveUser(ctx, user)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
			user.UserName = claims.Provider + ":" + claims.Subject
			user, err = s.SaveUser(ctx, user)
		}
	} else if err == nil && user.AuthProvider == "" {
		_, err = s.db.ExecContext(ctx, `
UPDATE identities
SET auth_provider = ?, auth_subject = ?, updated_at = ?
WHERE id = ? AND auth_provider = '' AND auth_subject = ''`,
			claims.Provider, claims.Subject, formatTime(time.Now().UTC()), user.ID)
		user.AuthProvider = claims.Provider
		user.AuthSubject = claims.Subject
	}
	if err != nil {
		return identity.Resolution{}, err
	}
	if !user.Active {
		return identity.Resolution{}, identity.ErrDeprovisioned
	}
	role := identity.NormalizeRole(user.Role)
	groupRoles, err := s.identityGroupRoles(ctx, user.ID, claims.Provider, claims.Groups)
	if err != nil {
		return identity.Resolution{}, err
	}
	for _, groupRole := range groupRoles {
		role = identity.MaxRole(role, groupRole)
	}
	return identity.Resolution{User: user, Role: role, Known: known || user.SCIMManaged}, nil
}

func (s *Store) identityByClaims(ctx context.Context, claims identity.Claims) (identity.User, error) {
	row := s.db.QueryRowContext(ctx, identitySelect+`
WHERE auth_provider = ? AND auth_subject = ?`,
		claims.Provider, claims.Subject)
	user, err := scanIdentity(row)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return user, err
	}
	// A SCIM user may arrive before its first provider login. Prefer the stable
	// external ID; email/userName is only a case-insensitive fallback.
	row = s.db.QueryRowContext(ctx, identitySelect+`
WHERE scim_managed = 1 AND auth_provider = '' AND (
    external_id = ? OR (? <> '' AND lower(email) = lower(?)) OR
    (? <> '' AND lower(user_name) = lower(?))
)
ORDER BY CASE WHEN external_id = ? THEN 0 ELSE 1 END
LIMIT 1`,
		claims.Subject, claims.Email, claims.Email, claims.Email, claims.Email, claims.Subject)
	return scanIdentity(row)
}

func (s *Store) identityGroupRoles(ctx context.Context, userID, provider string, providerGroups []string) ([]identity.Role, error) {
	roles := make([]identity.Role, 0)
	rows, err := s.db.QueryContext(ctx, `
SELECT g.role
FROM identity_groups g
JOIN identity_group_members m ON m.group_id = g.id
WHERE m.user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var role identity.Role
		if err := rows.Scan(&role); err != nil {
			rows.Close()
			return nil, err
		}
		roles = append(roles, role)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, group := range providerGroups {
		var role identity.Role
		err := s.db.QueryRowContext(ctx, `
SELECT role FROM identity_role_mappings
WHERE provider = ? AND lower(group_value) = lower(?)`, provider, strings.TrimSpace(group)).Scan(&role)
		if err == nil {
			roles = append(roles, role)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return roles, nil
}

const identitySelect = `
SELECT id, external_id, user_name, display_name, email, auth_provider,
       auth_subject, active, role, scim_managed, created_at, updated_at
FROM identities `

func (s *Store) ListUsers(ctx context.Context, start, count int) ([]identity.User, int, error) {
	start, count = boundedPage(start, count)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identities`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, identitySelect+`
ORDER BY user_name COLLATE NOCASE LIMIT ? OFFSET ?`, count, start)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	users := make([]identity.User, 0)
	for rows.Next() {
		user, err := scanIdentity(rows)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, user)
	}
	return users, total, rows.Err()
}

func (s *Store) User(ctx context.Context, id string) (identity.User, error) {
	return scanIdentity(s.db.QueryRowContext(ctx, identitySelect+`WHERE id = ?`, strings.TrimSpace(id)))
}

func (s *Store) SaveUser(ctx context.Context, user identity.User) (identity.User, error) {
	user.ID = strings.TrimSpace(user.ID)
	if user.ID == "" {
		user.ID = newStoreID("usr")
	}
	user.ExternalID = strings.TrimSpace(user.ExternalID)
	user.UserName = strings.TrimSpace(user.UserName)
	user.DisplayName = strings.TrimSpace(user.DisplayName)
	user.Email = strings.TrimSpace(user.Email)
	user.AuthProvider = strings.TrimSpace(user.AuthProvider)
	user.AuthSubject = strings.TrimSpace(user.AuthSubject)
	user.Role = identity.NormalizeRole(user.Role)
	if user.UserName == "" {
		return identity.User{}, errors.New("userName is required")
	}
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO identities (
    id, external_id, user_name, display_name, email, auth_provider,
    auth_subject, active, role, scim_managed, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    external_id = excluded.external_id,
    user_name = excluded.user_name,
    display_name = excluded.display_name,
    email = excluded.email,
    auth_provider = excluded.auth_provider,
    auth_subject = excluded.auth_subject,
    active = excluded.active,
    role = excluded.role,
    scim_managed = excluded.scim_managed,
    updated_at = excluded.updated_at`,
		user.ID, user.ExternalID, user.UserName, user.DisplayName, user.Email,
		user.AuthProvider, user.AuthSubject, user.Active, user.Role,
		user.SCIMManaged, formatTime(user.CreatedAt), formatTime(user.UpdatedAt))
	if err != nil {
		return identity.User{}, fmt.Errorf("save identity: %w", err)
	}
	return s.User(ctx, user.ID)
}

// DeleteUser suspends the identity instead of erasing historical authorship.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE identities SET active = 0, updated_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC()), strings.TrimSpace(id))
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListGroups(ctx context.Context, start, count int) ([]identity.Group, int, error) {
	start, count = boundedPage(start, count)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_groups`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, external_id, display_name, role, created_at, updated_at
FROM identity_groups ORDER BY display_name COLLATE NOCASE LIMIT ? OFFSET ?`, count, start)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	groups := make([]identity.Group, 0)
	for rows.Next() {
		var group identity.Group
		var created, updated string
		if err := rows.Scan(&group.ID, &group.ExternalID, &group.DisplayName, &group.Role, &created, &updated); err != nil {
			return nil, 0, err
		}
		group.CreatedAt, group.UpdatedAt = parseTime(created), parseTime(updated)
		members, err := s.groupMembers(ctx, group.ID)
		if err != nil {
			return nil, 0, err
		}
		group.Members = members
		groups = append(groups, group)
	}
	return groups, total, rows.Err()
}

func (s *Store) Group(ctx context.Context, id string) (identity.Group, error) {
	var group identity.Group
	var created, updated string
	err := s.db.QueryRowContext(ctx, `
SELECT id, external_id, display_name, role, created_at, updated_at
FROM identity_groups WHERE id = ?`, strings.TrimSpace(id)).
		Scan(&group.ID, &group.ExternalID, &group.DisplayName, &group.Role, &created, &updated)
	if err != nil {
		return identity.Group{}, err
	}
	group.CreatedAt, group.UpdatedAt = parseTime(created), parseTime(updated)
	group.Members, err = s.groupMembers(ctx, group.ID)
	return group, err
}

func (s *Store) SaveGroup(ctx context.Context, group identity.Group) (identity.Group, error) {
	group.ID = strings.TrimSpace(group.ID)
	if group.ID == "" {
		group.ID = newStoreID("grp")
	}
	group.ExternalID = strings.TrimSpace(group.ExternalID)
	group.DisplayName = strings.TrimSpace(group.DisplayName)
	group.Role = identity.NormalizeRole(group.Role)
	if group.DisplayName == "" {
		return identity.Group{}, errors.New("group displayName is required")
	}
	now := time.Now().UTC()
	if group.CreatedAt.IsZero() {
		group.CreatedAt = now
	}
	group.UpdatedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity.Group{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
INSERT INTO identity_groups(id, external_id, display_name, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    external_id = excluded.external_id,
    display_name = excluded.display_name,
    role = excluded.role,
    updated_at = excluded.updated_at`,
		group.ID, group.ExternalID, group.DisplayName, group.Role,
		formatTime(group.CreatedAt), formatTime(group.UpdatedAt))
	if err != nil {
		return identity.Group{}, fmt.Errorf("save identity group: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM identity_group_members WHERE group_id = ?`, group.ID); err != nil {
		return identity.Group{}, err
	}
	seen := make(map[string]struct{}, len(group.Members))
	for _, member := range group.Members {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		if _, ok := seen[member]; ok {
			continue
		}
		seen[member] = struct{}{}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO identity_group_members(group_id, user_id) VALUES (?, ?)`, group.ID, member); err != nil {
			return identity.Group{}, fmt.Errorf("add identity group member: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return identity.Group{}, err
	}
	return s.Group(ctx, group.ID)
}

func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM identity_groups WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListRoleMappings(ctx context.Context) ([]identity.RoleMapping, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, provider, group_value, role, updated_at
FROM identity_role_mappings ORDER BY provider, group_value COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mappings []identity.RoleMapping
	for rows.Next() {
		var mapping identity.RoleMapping
		var updated string
		if err := rows.Scan(&mapping.ID, &mapping.Provider, &mapping.GroupValue, &mapping.Role, &updated); err != nil {
			return nil, err
		}
		mapping.UpdatedAt = parseTime(updated)
		mappings = append(mappings, mapping)
	}
	return mappings, rows.Err()
}

func (s *Store) SetRoleMapping(ctx context.Context, mapping identity.RoleMapping) error {
	mapping.Provider = strings.TrimSpace(mapping.Provider)
	mapping.GroupValue = strings.TrimSpace(mapping.GroupValue)
	mapping.Role = identity.NormalizeRole(mapping.Role)
	if mapping.Provider == "" || mapping.GroupValue == "" {
		return errors.New("provider and group are required")
	}
	if mapping.ID > 0 {
		result, err := s.db.ExecContext(ctx, `
UPDATE identity_role_mappings
SET provider = ?, group_value = ?, role = ?, updated_at = ?
WHERE id = ?`, mapping.Provider, mapping.GroupValue, mapping.Role,
			formatTime(time.Now().UTC()), mapping.ID)
		if err != nil {
			return err
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			return sql.ErrNoRows
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO identity_role_mappings(provider, group_value, role, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(provider, group_value) DO UPDATE SET
    role = excluded.role, updated_at = excluded.updated_at`,
		mapping.Provider, mapping.GroupValue, mapping.Role, formatTime(time.Now().UTC()))
	return err
}

func (s *Store) DeleteRoleMapping(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM identity_role_mappings WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanIdentity(row rowScanner) (identity.User, error) {
	var user identity.User
	var created, updated string
	err := row.Scan(
		&user.ID, &user.ExternalID, &user.UserName, &user.DisplayName, &user.Email,
		&user.AuthProvider, &user.AuthSubject, &user.Active, &user.Role,
		&user.SCIMManaged, &created, &updated,
	)
	user.CreatedAt, user.UpdatedAt = parseTime(created), parseTime(updated)
	return user, err
}

func (s *Store) groupMembers(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT user_id FROM identity_group_members WHERE group_id = ? ORDER BY user_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []string
	for rows.Next() {
		var member string
		if err := rows.Scan(&member); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func newStoreID(prefix string) string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UTC().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(value[:])
}

func boundedPage(start, count int) (int, int) {
	if start < 0 {
		start = 0
	}
	if count <= 0 {
		count = 100
	}
	if count > 500 {
		count = 500
	}
	return start, count
}

func firstIdentityValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "user"
}
