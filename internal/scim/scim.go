// Package scim implements RepoKarta's bounded SCIM 2.0 provisioning surface.
package scim

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/identity"
)

const (
	coreUserSchema  = "urn:ietf:params:scim:schemas:core:2.0:User"
	coreGroupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
	listSchema      = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	errorSchema     = "urn:ietf:params:scim:api:messages:2.0:Error"
	patchSchema     = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	groupExtension  = "urn:repokarta:params:scim:schemas:extension:2.0:Group"
)

type Service struct {
	store identity.Store
	audit audit.Recorder
	token [32]byte
}

func New(store identity.Store, recorder audit.Recorder, token string) (*Service, error) {
	if store == nil {
		return nil, errors.New("SCIM identity store is required")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, nil
	}
	if len(token) < 24 {
		return nil, errors.New("SCIM bearer token must contain at least 24 characters")
	}
	return &Service{store: store, audit: recorder, token: sha256.Sum256([]byte(token))}, nil
}

func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Service) serveHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "application/scim+json")
	if !s.authenticate(request) {
		response.Header().Set("WWW-Authenticate", `Bearer realm="RepoKarta SCIM"`)
		s.record(request.Context(), "scim.authenticate", "service", "", "failure", nil)
		writeError(response, http.StatusUnauthorized, "invalid or missing SCIM bearer token", "")
		return
	}
	path := strings.Trim(strings.TrimPrefix(request.URL.Path, "/scim/v2"), "/")
	switch {
	case request.Method == http.MethodGet && path == "ServiceProviderConfig":
		writeJSON(response, http.StatusOK, serviceProviderConfig())
	case request.Method == http.MethodGet && path == "ResourceTypes":
		writeJSON(response, http.StatusOK, resourceTypes())
	case request.Method == http.MethodGet && path == "Schemas":
		writeJSON(response, http.StatusOK, schemas())
	case path == "Users" || strings.HasPrefix(path, "Users/"):
		s.users(response, request, strings.TrimPrefix(path, "Users/"), path == "Users")
	case path == "Groups" || strings.HasPrefix(path, "Groups/"):
		s.groups(response, request, strings.TrimPrefix(path, "Groups/"), path == "Groups")
	default:
		writeError(response, http.StatusNotFound, "SCIM resource not found", "")
	}
}

func (s *Service) authenticate(request *http.Request) bool {
	header := strings.TrimSpace(request.Header.Get("Authorization"))
	if len(header) < 8 || !strings.EqualFold(header[:7], "Bearer ") {
		return false
	}
	actual := sha256.Sum256([]byte(strings.TrimSpace(header[7:])))
	return subtle.ConstantTimeCompare(actual[:], s.token[:]) == 1
}

type scimEmail struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
}

type scimRole struct {
	Value string `json:"value"`
}

type scimMeta struct {
	ResourceType string    `json:"resourceType"`
	Created      time.Time `json:"created"`
	LastModified time.Time `json:"lastModified"`
	Location     string    `json:"location"`
}

type scimUser struct {
	Schemas     []string    `json:"schemas"`
	ID          string      `json:"id,omitempty"`
	ExternalID  string      `json:"externalId,omitempty"`
	UserName    string      `json:"userName"`
	DisplayName string      `json:"displayName,omitempty"`
	Active      *bool       `json:"active,omitempty"`
	Emails      []scimEmail `json:"emails,omitempty"`
	Roles       []scimRole  `json:"roles,omitempty"`
	Meta        *scimMeta   `json:"meta,omitempty"`
}

type scimMember struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
}

type scimGroup struct {
	Schemas     []string       `json:"schemas"`
	ID          string         `json:"id,omitempty"`
	ExternalID  string         `json:"externalId,omitempty"`
	DisplayName string         `json:"displayName"`
	Members     []scimMember   `json:"members,omitempty"`
	Extension   map[string]any `json:"urn:repokarta:params:scim:schemas:extension:2.0:Group,omitempty"`
	Meta        *scimMeta      `json:"meta,omitempty"`
}

type listResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    any      `json:"Resources"`
}

func (s *Service) users(response http.ResponseWriter, request *http.Request, id string, collection bool) {
	switch request.Method {
	case http.MethodGet:
		if collection {
			s.listUsers(response, request)
			return
		}
		user, err := s.store.User(request.Context(), id)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, userResource(request, user))
	case http.MethodPost:
		if !collection {
			writeError(response, http.StatusMethodNotAllowed, "POST requires the Users collection", "")
			return
		}
		var input scimUser
		if !decode(response, request, &input) {
			return
		}
		user, err := s.store.SaveUser(request.Context(), userFromResource(input, ""))
		if err != nil {
			writeStoreError(response, err)
			return
		}
		s.record(request.Context(), "scim.user.create", "identity", user.ID, "success", map[string]string{"active": strconv.FormatBool(user.Active)})
		response.Header().Set("Location", resourceURL(request, "Users", user.ID))
		writeJSON(response, http.StatusCreated, userResource(request, user))
	case http.MethodPut:
		if collection {
			writeError(response, http.StatusMethodNotAllowed, "PUT requires a user ID", "")
			return
		}
		existing, err := s.store.User(request.Context(), id)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		var input scimUser
		if !decode(response, request, &input) {
			return
		}
		user := userFromResource(input, id)
		user.CreatedAt = existing.CreatedAt
		user.AuthProvider, user.AuthSubject = existing.AuthProvider, existing.AuthSubject
		user, err = s.store.SaveUser(request.Context(), user)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		s.record(request.Context(), "scim.user.replace", "identity", user.ID, "success", map[string]string{"active": strconv.FormatBool(user.Active), "role": string(user.Role)})
		writeJSON(response, http.StatusOK, userResource(request, user))
	case http.MethodPatch:
		if collection {
			writeError(response, http.StatusMethodNotAllowed, "PATCH requires a user ID", "")
			return
		}
		user, err := s.store.User(request.Context(), id)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		var patch patchRequest
		if !decode(response, request, &patch) {
			return
		}
		if err := applyUserPatch(&user, patch); err != nil {
			writeError(response, http.StatusBadRequest, err.Error(), "invalidValue")
			return
		}
		user.SCIMManaged = true
		user, err = s.store.SaveUser(request.Context(), user)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		s.record(request.Context(), "scim.user.patch", "identity", user.ID, "success", map[string]string{"active": strconv.FormatBool(user.Active), "role": string(user.Role)})
		writeJSON(response, http.StatusOK, userResource(request, user))
	case http.MethodDelete:
		if collection {
			writeError(response, http.StatusMethodNotAllowed, "DELETE requires a user ID", "")
			return
		}
		if err := s.store.DeleteUser(request.Context(), id); err != nil {
			writeStoreError(response, err)
			return
		}
		s.record(request.Context(), "scim.user.deprovision", "identity", id, "success", nil)
		response.WriteHeader(http.StatusNoContent)
	default:
		writeError(response, http.StatusMethodNotAllowed, "method not allowed", "")
	}
}

func (s *Service) listUsers(response http.ResponseWriter, request *http.Request) {
	start, count := pageParameters(request)
	filter := strings.TrimSpace(request.URL.Query().Get("filter"))
	if filter == "" {
		users, total, err := s.store.ListUsers(request.Context(), start, count)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		resources := make([]scimUser, 0, len(users))
		for _, user := range users {
			resources = append(resources, userResource(request, user))
		}
		writeJSON(response, http.StatusOK, listResponse{
			Schemas: []string{listSchema}, TotalResults: total,
			StartIndex: start + 1, ItemsPerPage: len(resources), Resources: resources,
		})
		return
	}
	users, err := s.allUsers(request.Context())
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error(), "tooMany")
		return
	}
	users = filterUsers(users, filter)
	total := len(users)
	users = slicePage(users, start, count)
	resources := make([]scimUser, 0, len(users))
	for _, user := range users {
		resources = append(resources, userResource(request, user))
	}
	writeJSON(response, http.StatusOK, listResponse{
		Schemas: []string{listSchema}, TotalResults: total,
		StartIndex: start + 1, ItemsPerPage: len(resources), Resources: resources,
	})
}

func (s *Service) groups(response http.ResponseWriter, request *http.Request, id string, collection bool) {
	switch request.Method {
	case http.MethodGet:
		if collection {
			s.listGroups(response, request)
			return
		}
		group, err := s.store.Group(request.Context(), id)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, groupResource(request, group))
	case http.MethodPost:
		if !collection {
			writeError(response, http.StatusMethodNotAllowed, "POST requires the Groups collection", "")
			return
		}
		var input scimGroup
		if !decode(response, request, &input) {
			return
		}
		group, err := s.store.SaveGroup(request.Context(), groupFromResource(input, ""))
		if err != nil {
			writeStoreError(response, err)
			return
		}
		s.record(request.Context(), "scim.group.create", "identity-group", group.ID, "success", map[string]string{"members": strconv.Itoa(len(group.Members))})
		response.Header().Set("Location", resourceURL(request, "Groups", group.ID))
		writeJSON(response, http.StatusCreated, groupResource(request, group))
	case http.MethodPut:
		if collection {
			writeError(response, http.StatusMethodNotAllowed, "PUT requires a group ID", "")
			return
		}
		existing, err := s.store.Group(request.Context(), id)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		var input scimGroup
		if !decode(response, request, &input) {
			return
		}
		group := groupFromResource(input, id)
		group.CreatedAt = existing.CreatedAt
		group, err = s.store.SaveGroup(request.Context(), group)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		s.record(request.Context(), "scim.group.replace", "identity-group", group.ID, "success", map[string]string{"members": strconv.Itoa(len(group.Members)), "role": string(group.Role)})
		writeJSON(response, http.StatusOK, groupResource(request, group))
	case http.MethodPatch:
		if collection {
			writeError(response, http.StatusMethodNotAllowed, "PATCH requires a group ID", "")
			return
		}
		group, err := s.store.Group(request.Context(), id)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		var patch patchRequest
		if !decode(response, request, &patch) {
			return
		}
		if err := applyGroupPatch(&group, patch); err != nil {
			writeError(response, http.StatusBadRequest, err.Error(), "invalidValue")
			return
		}
		group, err = s.store.SaveGroup(request.Context(), group)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		s.record(request.Context(), "scim.group.patch", "identity-group", group.ID, "success", map[string]string{"members": strconv.Itoa(len(group.Members)), "role": string(group.Role)})
		writeJSON(response, http.StatusOK, groupResource(request, group))
	case http.MethodDelete:
		if collection {
			writeError(response, http.StatusMethodNotAllowed, "DELETE requires a group ID", "")
			return
		}
		if err := s.store.DeleteGroup(request.Context(), id); err != nil {
			writeStoreError(response, err)
			return
		}
		s.record(request.Context(), "scim.group.delete", "identity-group", id, "success", nil)
		response.WriteHeader(http.StatusNoContent)
	default:
		writeError(response, http.StatusMethodNotAllowed, "method not allowed", "")
	}
}

func (s *Service) listGroups(response http.ResponseWriter, request *http.Request) {
	start, count := pageParameters(request)
	filter := strings.TrimSpace(request.URL.Query().Get("filter"))
	if filter == "" {
		groups, total, err := s.store.ListGroups(request.Context(), start, count)
		if err != nil {
			writeStoreError(response, err)
			return
		}
		resources := make([]scimGroup, 0, len(groups))
		for _, group := range groups {
			resources = append(resources, groupResource(request, group))
		}
		writeJSON(response, http.StatusOK, listResponse{
			Schemas: []string{listSchema}, TotalResults: total,
			StartIndex: start + 1, ItemsPerPage: len(resources), Resources: resources,
		})
		return
	}
	groups, err := s.allGroups(request.Context())
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error(), "tooMany")
		return
	}
	groups = filterGroups(groups, filter)
	total := len(groups)
	groups = slicePage(groups, start, count)
	resources := make([]scimGroup, 0, len(groups))
	for _, group := range groups {
		resources = append(resources, groupResource(request, group))
	}
	writeJSON(response, http.StatusOK, listResponse{
		Schemas: []string{listSchema}, TotalResults: total,
		StartIndex: start + 1, ItemsPerPage: len(resources), Resources: resources,
	})
}

func (s *Service) allUsers(ctx context.Context) ([]identity.User, error) {
	users, total, err := s.store.ListUsers(ctx, 0, 500)
	if err != nil {
		return nil, err
	}
	if total > 10000 {
		return nil, errors.New("SCIM user filter evaluation is bounded to 10000 identities")
	}
	for offset := len(users); offset < total; offset += 500 {
		page, _, err := s.store.ListUsers(ctx, offset, 500)
		if err != nil {
			return nil, err
		}
		users = append(users, page...)
	}
	return users, nil
}

func (s *Service) allGroups(ctx context.Context) ([]identity.Group, error) {
	groups, total, err := s.store.ListGroups(ctx, 0, 500)
	if err != nil {
		return nil, err
	}
	if total > 10000 {
		return nil, errors.New("SCIM group filter evaluation is bounded to 10000 groups")
	}
	for offset := len(groups); offset < total; offset += 500 {
		page, _, err := s.store.ListGroups(ctx, offset, 500)
		if err != nil {
			return nil, err
		}
		groups = append(groups, page...)
	}
	return groups, nil
}

type patchRequest struct {
	Schemas    []string         `json:"schemas"`
	Operations []patchOperation `json:"Operations"`
}

type patchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func applyUserPatch(user *identity.User, patch patchRequest) error {
	for _, operation := range patch.Operations {
		op := strings.ToLower(strings.TrimSpace(operation.Op))
		path := strings.ToLower(strings.TrimSpace(operation.Path))
		if op != "add" && op != "replace" && op != "remove" {
			return fmt.Errorf("unsupported patch operation %q", operation.Op)
		}
		switch path {
		case "active":
			if op == "remove" {
				user.Active = false
			} else if err := json.Unmarshal(operation.Value, &user.Active); err != nil {
				return errors.New("active must be boolean")
			}
		case "username":
			if err := patchString(&user.UserName, op, operation.Value); err != nil {
				return err
			}
		case "displayname":
			if err := patchString(&user.DisplayName, op, operation.Value); err != nil {
				return err
			}
		case "externalid":
			if err := patchString(&user.ExternalID, op, operation.Value); err != nil {
				return err
			}
		case "emails", "emails[type eq \"work\"].value":
			if op == "remove" {
				user.Email = ""
				continue
			}
			var emails []scimEmail
			if err := json.Unmarshal(operation.Value, &emails); err == nil && len(emails) > 0 {
				user.Email = emails[0].Value
				continue
			}
			if err := json.Unmarshal(operation.Value, &user.Email); err != nil {
				return errors.New("email value is invalid")
			}
		case "roles":
			if op == "remove" {
				user.Role = identity.RoleReader
				continue
			}
			var roles []scimRole
			if err := json.Unmarshal(operation.Value, &roles); err != nil || len(roles) == 0 {
				return errors.New("roles must contain a value")
			}
			user.Role = identity.NormalizeRole(identity.Role(roles[0].Value))
		default:
			if path == "" {
				var replacement scimUser
				if err := json.Unmarshal(operation.Value, &replacement); err != nil {
					return errors.New("patch value is invalid")
				}
				updated := userFromResource(replacement, user.ID)
				updated.CreatedAt = user.CreatedAt
				updated.AuthProvider, updated.AuthSubject = user.AuthProvider, user.AuthSubject
				*user = updated
			} else {
				return fmt.Errorf("unsupported user patch path %q", operation.Path)
			}
		}
	}
	return nil
}

func applyGroupPatch(group *identity.Group, patch patchRequest) error {
	for _, operation := range patch.Operations {
		op := strings.ToLower(strings.TrimSpace(operation.Op))
		path := strings.ToLower(strings.TrimSpace(operation.Path))
		switch {
		case path == "displayname":
			if err := patchString(&group.DisplayName, op, operation.Value); err != nil {
				return err
			}
		case path == "externalid":
			if err := patchString(&group.ExternalID, op, operation.Value); err != nil {
				return err
			}
		case path == strings.ToLower(groupExtension+":role"):
			var role string
			if op == "remove" {
				group.Role = identity.RoleReader
			} else if err := json.Unmarshal(operation.Value, &role); err != nil {
				return errors.New("group role must be a string")
			} else {
				group.Role = identity.NormalizeRole(identity.Role(role))
			}
		case strings.HasPrefix(path, "members"):
			var members []scimMember
			if len(operation.Value) > 0 && string(operation.Value) != "null" {
				if err := json.Unmarshal(operation.Value, &members); err != nil {
					var wrapper struct {
						Members []scimMember `json:"members"`
					}
					if err := json.Unmarshal(operation.Value, &wrapper); err != nil {
						return errors.New("group members are invalid")
					}
					members = wrapper.Members
				}
			}
			switch op {
			case "replace":
				group.Members = memberValues(members)
			case "add":
				group.Members = appendUnique(group.Members, memberValues(members)...)
			case "remove":
				remove := make(map[string]struct{})
				for _, member := range memberValues(members) {
					remove[member] = struct{}{}
				}
				if marker := memberFilterValue(path); marker != "" {
					remove[marker] = struct{}{}
				}
				var retained []string
				for _, member := range group.Members {
					if _, ok := remove[member]; !ok {
						retained = append(retained, member)
					}
				}
				group.Members = retained
			default:
				return fmt.Errorf("unsupported patch operation %q", operation.Op)
			}
		default:
			return fmt.Errorf("unsupported group patch path %q", operation.Path)
		}
	}
	return nil
}

func userFromResource(resource scimUser, id string) identity.User {
	active := true
	if resource.Active != nil {
		active = *resource.Active
	}
	role := identity.RoleReader
	if len(resource.Roles) > 0 {
		role = identity.NormalizeRole(identity.Role(resource.Roles[0].Value))
	}
	email := ""
	if len(resource.Emails) > 0 {
		email = resource.Emails[0].Value
		for _, candidate := range resource.Emails {
			if candidate.Primary {
				email = candidate.Value
				break
			}
		}
	}
	return identity.User{
		ID: id, ExternalID: resource.ExternalID, UserName: resource.UserName,
		DisplayName: resource.DisplayName, Email: email, Active: active,
		Role: role, SCIMManaged: true,
	}
}

func groupFromResource(resource scimGroup, id string) identity.Group {
	role := identity.RoleReader
	if value, ok := resource.Extension["role"].(string); ok {
		role = identity.NormalizeRole(identity.Role(value))
	}
	return identity.Group{
		ID: id, ExternalID: resource.ExternalID, DisplayName: resource.DisplayName,
		Role: role, Members: memberValues(resource.Members),
	}
}

func userResource(request *http.Request, user identity.User) scimUser {
	active := user.Active
	resource := scimUser{
		Schemas: []string{coreUserSchema}, ID: user.ID, ExternalID: user.ExternalID,
		UserName: user.UserName, DisplayName: user.DisplayName, Active: &active,
		Roles: []scimRole{{Value: string(identity.NormalizeRole(user.Role))}},
		Meta:  &scimMeta{ResourceType: "User", Created: user.CreatedAt, LastModified: user.UpdatedAt, Location: resourceURL(request, "Users", user.ID)},
	}
	if user.Email != "" {
		resource.Emails = []scimEmail{{Value: user.Email, Primary: true}}
	}
	return resource
}

func groupResource(request *http.Request, group identity.Group) scimGroup {
	members := make([]scimMember, 0, len(group.Members))
	for _, member := range group.Members {
		members = append(members, scimMember{Value: member})
	}
	return scimGroup{
		Schemas: []string{coreGroupSchema, groupExtension}, ID: group.ID,
		ExternalID: group.ExternalID, DisplayName: group.DisplayName, Members: members,
		Extension: map[string]any{"role": identity.NormalizeRole(group.Role)},
		Meta:      &scimMeta{ResourceType: "Group", Created: group.CreatedAt, LastModified: group.UpdatedAt, Location: resourceURL(request, "Groups", group.ID)},
	}
}

func (s *Service) record(ctx context.Context, action, targetType, targetID, outcome string, metadata map[string]string) {
	if s.audit == nil {
		return
	}
	if err := s.audit.AppendAuditEvent(ctx, audit.Event{
		ActorID: "scim:provisioner", ActorName: "SCIM provisioner",
		Action: action, TargetType: targetType, TargetID: targetID,
		Outcome: outcome, Provider: "scim",
		CorrelationID: audit.CorrelationID(ctx), Metadata: metadata,
	}); err != nil {
		slog.Error("append SCIM audit event", "action", action, "error", err)
	}
}

func decode(response http.ResponseWriter, request *http.Request, output any) bool {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(output); err != nil {
		writeError(response, http.StatusBadRequest, "invalid SCIM JSON document", "invalidSyntax")
		return false
	}
	return true
}

func writeStoreError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(response, http.StatusNotFound, "SCIM resource not found", "")
	case strings.Contains(strings.ToLower(err.Error()), "unique"):
		writeError(response, http.StatusConflict, "SCIM resource conflicts with an existing stable identifier", "uniqueness")
	default:
		writeError(response, http.StatusBadRequest, err.Error(), "invalidValue")
	}
}

func writeError(response http.ResponseWriter, status int, detail, scimType string) {
	writeJSON(response, status, map[string]any{
		"schemas": []string{errorSchema}, "status": strconv.Itoa(status),
		"detail": detail, "scimType": scimType,
	})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func pageParameters(request *http.Request) (int, int) {
	start, _ := strconv.Atoi(request.URL.Query().Get("startIndex"))
	if start < 1 {
		start = 1
	}
	count, _ := strconv.Atoi(request.URL.Query().Get("count"))
	if count <= 0 {
		count = 100
	}
	if count > 500 {
		count = 500
	}
	return start - 1, count
}

func filterUsers(users []identity.User, filter string) []identity.User {
	attribute, value := eqFilter(filter)
	if attribute == "" {
		return users
	}
	var output []identity.User
	for _, user := range users {
		candidate := ""
		switch strings.ToLower(attribute) {
		case "id":
			candidate = user.ID
		case "externalid":
			candidate = user.ExternalID
		case "username":
			candidate = user.UserName
		case "emails.value":
			candidate = user.Email
		default:
			return []identity.User{}
		}
		if strings.EqualFold(candidate, value) {
			output = append(output, user)
		}
	}
	return output
}

func filterGroups(groups []identity.Group, filter string) []identity.Group {
	attribute, value := eqFilter(filter)
	if attribute == "" {
		return groups
	}
	var output []identity.Group
	for _, group := range groups {
		candidate := ""
		switch strings.ToLower(attribute) {
		case "id":
			candidate = group.ID
		case "externalid":
			candidate = group.ExternalID
		case "displayname":
			candidate = group.DisplayName
		default:
			return []identity.Group{}
		}
		if strings.EqualFold(candidate, value) {
			output = append(output, group)
		}
	}
	return output
}

func eqFilter(filter string) (string, string) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return "", ""
	}
	parts := strings.SplitN(filter, " eq ", 2)
	if len(parts) != 2 {
		return "__unsupported__", ""
	}
	return strings.TrimSpace(parts[0]), strings.Trim(strings.TrimSpace(parts[1]), `"`)
}

func slicePage[T any](values []T, start, count int) []T {
	if start >= len(values) {
		return []T{}
	}
	end := start + count
	if end > len(values) {
		end = len(values)
	}
	return values[start:end]
}

func memberValues(members []scimMember) []string {
	values := make([]string, 0, len(members))
	for _, member := range members {
		if value := strings.TrimSpace(member.Value); value != "" {
			values = appendUnique(values, value)
		}
	}
	return values
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if _, ok := seen[value]; !ok && value != "" {
			seen[value] = struct{}{}
			values = append(values, value)
		}
	}
	return values
}

func memberFilterValue(path string) string {
	start := strings.Index(path, `value eq "`)
	if start < 0 {
		return ""
	}
	value := path[start+len(`value eq "`):]
	if end := strings.Index(value, `"`); end >= 0 {
		return value[:end]
	}
	return ""
}

func patchString(target *string, op string, raw json.RawMessage) error {
	if op == "remove" {
		*target = ""
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return errors.New("patch value must be a string")
	}
	return nil
}

func resourceURL(request *http.Request, kind, id string) string {
	scheme := "https"
	if request.TLS == nil {
		scheme = "http"
	}
	if forwarded := strings.TrimSpace(request.Header.Get("X-Forwarded-Proto")); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	return scheme + "://" + request.Host + "/scim/v2/" + kind + "/" + id
}

func serviceProviderConfig() map[string]any {
	return map[string]any{
		"schemas":        []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":          map[string]bool{"supported": true},
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": 500},
		"changePassword": map[string]bool{"supported": false},
		"sort":           map[string]bool{"supported": false},
		"etag":           map[string]bool{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type": "oauthbearertoken", "name": "Bearer token",
			"description": "Startup-configured RepoKarta SCIM bearer token",
			"specUri":     "https://www.rfc-editor.org/rfc/rfc6750",
			"primary":     true,
		}},
	}
}

func resourceTypes() listResponse {
	resources := []map[string]any{
		{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": "User", "name": "User", "endpoint": "/Users", "schema": coreUserSchema},
		{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": "Group", "name": "Group", "endpoint": "/Groups", "schema": coreGroupSchema},
	}
	return listResponse{Schemas: []string{listSchema}, TotalResults: len(resources), StartIndex: 1, ItemsPerPage: len(resources), Resources: resources}
}

func schemas() listResponse {
	resources := []map[string]any{
		{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:Schema"}, "id": coreUserSchema, "name": "User", "description": "RepoKarta user"},
		{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:Schema"}, "id": coreGroupSchema, "name": "Group", "description": "RepoKarta group"},
		{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:Schema"}, "id": groupExtension, "name": "RepoKartaGroup", "description": "RepoKarta group role extension"},
	}
	return listResponse{Schemas: []string{listSchema}, TotalResults: len(resources), StartIndex: 1, ItemsPerPage: len(resources), Resources: resources}
}
