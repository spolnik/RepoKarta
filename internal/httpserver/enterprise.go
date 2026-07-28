package httpserver

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/internal/security"
)

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (writer *statusResponseWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *statusResponseWriter) Write(content []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	return writer.ResponseWriter.Write(content)
}

func (writer *statusResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *statusResponseWriter) Flush() {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	_ = http.NewResponseController(writer.ResponseWriter).Flush()
}

func correlationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		correlationID := strings.TrimSpace(request.Header.Get("X-Request-ID"))
		if correlationID == "" || len(correlationID) > 128 {
			correlationID = audit.NewCorrelationID()
		}
		response.Header().Set("X-Request-ID", correlationID)
		ctx := audit.WithCorrelationID(request.Context(), correlationID)
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

// controlled enforces one role permission and records the outcome. It is used
// inside authentication middleware so the verified principal is available.
func (s *Server) controlled(permission identity.Permission, action, targetType string, handler http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		principal, ok := security.PrincipalFromContext(request.Context())
		if s.security != nil && (!ok || !identity.Allows(principal.Role, permission)) {
			s.recordApplicationEvent(request, principal, "authorization.denied", targetType, request.URL.Path, "denied", map[string]string{
				"permission": string(permission),
				"method":     request.Method,
			})
			if strings.HasPrefix(request.URL.Path, "/api/") {
				writeAPIError(response, http.StatusForbidden, errors.New("permission denied: "+string(permission)))
			} else {
				http.Error(response, "Permission denied", http.StatusForbidden)
			}
			return
		}
		tracker := &statusResponseWriter{ResponseWriter: response}
		handler(tracker, request)
		status := tracker.status
		if status == 0 {
			status = http.StatusOK
		}
		outcome := "success"
		if status >= 400 {
			outcome = "failure"
		}
		s.recordApplicationEvent(request, principal, action, targetType, requestTarget(request), outcome, map[string]string{
			"status": strconv.Itoa(status),
			"method": request.Method,
		})
	}
}

func (s *Server) requirePermission(response http.ResponseWriter, request *http.Request, permission identity.Permission) (security.Principal, bool) {
	principal, ok := security.PrincipalFromContext(request.Context())
	if s.security == nil || (ok && identity.Allows(principal.Role, permission)) {
		return principal, true
	}
	s.recordApplicationEvent(request, principal, "authorization.denied", "request", request.URL.Path, "denied", map[string]string{
		"permission": string(permission), "method": request.Method,
	})
	writeAPIError(response, http.StatusForbidden, errors.New("permission denied: "+string(permission)))
	return principal, false
}

func requestTarget(request *http.Request) string {
	for _, name := range []string{"conversationID", "repositoryID", "repository", "page", "userID", "groupID", "mappingID"} {
		if value := strings.TrimSpace(request.PathValue(name)); value != "" {
			return value
		}
	}
	if repository := strings.TrimSpace(request.URL.Query().Get("repository")); repository != "" {
		return repository
	}
	return request.URL.Path
}

func (s *Server) apiIdentityAdministration(response http.ResponseWriter, request *http.Request) {
	users, userTotal, err := s.enterprise.ListUsers(request.Context(), 0, 500)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	groups, groupTotal, err := s.enterprise.ListGroups(request.Context(), 0, 500)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	mappings, err := s.enterprise.ListRoleMappings(request.Context())
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"users": users, "user_total": userTotal, "users_truncated": userTotal > len(users),
		"groups": groups, "group_total": groupTotal, "groups_truncated": groupTotal > len(groups),
		"role_mappings": mappings,
	})
}

func (s *Server) apiUpdateUserRole(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	var input struct {
		Role identity.Role `json:"role"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid user role"))
		return
	}
	if !identity.ValidRole(input.Role) {
		writeAPIError(response, http.StatusBadRequest, errors.New("unknown user role"))
		return
	}
	user, err := s.enterprise.User(request.Context(), request.PathValue("userID"))
	if err == nil {
		user.Role = identity.NormalizeRole(input.Role)
		user, err = s.enterprise.SaveUser(request.Context(), user)
	}
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, user)
}

func (s *Server) apiUpdateGroupRole(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	var input struct {
		Role identity.Role `json:"role"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid group role"))
		return
	}
	if !identity.ValidRole(input.Role) {
		writeAPIError(response, http.StatusBadRequest, errors.New("unknown group role"))
		return
	}
	group, err := s.enterprise.Group(request.Context(), request.PathValue("groupID"))
	if err == nil {
		group.Role = identity.NormalizeRole(input.Role)
		group, err = s.enterprise.SaveGroup(request.Context(), group)
	}
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, group)
}

func (s *Server) apiRoleMappings(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		mappings, err := s.enterprise.ListRoleMappings(request.Context())
		if err != nil {
			writeAPIError(response, http.StatusInternalServerError, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"role_mappings": mappings})
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	var mapping identity.RoleMapping
	if err := json.NewDecoder(request.Body).Decode(&mapping); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid role mapping"))
		return
	}
	if !identity.ValidRole(mapping.Role) {
		writeAPIError(response, http.StatusBadRequest, errors.New("unknown role mapping role"))
		return
	}
	mapping.Role = identity.NormalizeRole(mapping.Role)
	if err := s.enterprise.SetRoleMapping(request.Context(), mapping); err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiDeleteRoleMapping(response http.ResponseWriter, request *http.Request) {
	id, err := strconv.ParseInt(request.PathValue("mappingID"), 10, 64)
	if err == nil {
		err = s.enterprise.DeleteRoleMapping(request.Context(), id)
	}
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid role mapping"))
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiSecuritySettings(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		writeJSON(response, http.StatusOK, map[string]any{
			"settings": s.security.Settings(), "provider_error": s.security.ProviderError(),
			"allow_open": s.security.AllowOpen(),
		})
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	var settings security.Settings
	if err := json.NewDecoder(request.Body).Decode(&settings); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid security settings"))
		return
	}
	if err := s.security.UpdateSettings(request.Context(), settings); err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, settings)
}

func (s *Server) apiAuditRetention(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		retention, err := s.enterprise.AuditRetention(request.Context())
		if err != nil {
			writeAPIError(response, http.StatusInternalServerError, err)
			return
		}
		writeJSON(response, http.StatusOK, retention)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	var input struct {
		Days      int `json:"days"`
		MaxEvents int `json:"max_events"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid audit retention"))
		return
	}
	removed, err := s.enterprise.SetAuditRetention(request.Context(), input.Days, input.MaxEvents)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"removed_events": removed})
}

func (s *Server) recordApplicationEvent(request *http.Request, principal security.Principal, action, targetType, targetID, outcome string, metadata map[string]string) {
	if s.enterprise == nil {
		return
	}
	actorID := access.IdentityID(principal.Provider, principal.ID)
	if principal.ID == "" {
		actorID = "unknown"
	}
	if err := s.enterprise.AppendAuditEvent(request.Context(), audit.Event{
		ActorID: actorID, ActorName: principal.Name, Action: action,
		TargetType: targetType, TargetID: targetID, Outcome: outcome,
		Provider: principal.Provider, CorrelationID: audit.CorrelationID(request.Context()),
		Metadata: metadata,
	}); err != nil {
		slog.Error("append application audit event", "action", action, "error", err)
	}
}

func (s *Server) recordAdminEvent(request *http.Request, action, targetType, targetID, outcome string, metadata map[string]string) {
	if s.enterprise == nil {
		return
	}
	if err := s.enterprise.AppendAuditEvent(request.Context(), audit.Event{
		ActorID: "bootstrap:admin", ActorName: "Bootstrap administrator",
		Action: action, TargetType: targetType, TargetID: targetID,
		Outcome: outcome, Provider: "bootstrap",
		CorrelationID: audit.CorrelationID(request.Context()), Metadata: metadata,
	}); err != nil {
		slog.Error("append administrator audit event", "action", action, "error", err)
	}
}

func (s *Server) recordCrossAuthorConversation(request *http.Request, conversation agent.Conversation, action, outcome string) {
	viewer := s.conversationViewer(request.Context())
	if conversation.Author.ID == viewer.Author.ID {
		return
	}
	principal, _ := security.PrincipalFromContext(request.Context())
	s.recordApplicationEvent(request, principal, action, "conversation", conversation.ID, outcome, map[string]string{
		"author_id": conversation.Author.ID,
	})
}

func (s *Server) apiAudit(response http.ResponseWriter, request *http.Request) {
	filter, err := auditFilter(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	page, err := s.enterprise.AuditEvents(request.Context(), filter)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, page)
}

func (s *Server) exportAudit(response http.ResponseWriter, request *http.Request) {
	filter, err := auditFilter(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	filter.Limit = audit.MaximumLimit
	var events []audit.Event
	truncated := false
	for len(events) < 50000 {
		previousBefore := filter.BeforeID
		page, err := s.enterprise.AuditEvents(request.Context(), filter)
		if err != nil {
			writeAPIError(response, http.StatusInternalServerError, err)
			return
		}
		events = append(events, page.Events...)
		if !page.Truncated {
			break
		}
		if page.NextBefore <= 0 || page.NextBefore == previousBefore ||
			(previousBefore > 0 && page.NextBefore >= previousBefore) {
			writeAPIError(response, http.StatusInternalServerError, errors.New("audit export pagination did not make progress"))
			return
		}
		filter.BeforeID = page.NextBefore
	}
	if len(events) >= 50000 {
		truncated = true
	}
	retention, err := s.enterprise.AuditRetention(request.Context())
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-RepoKarta-Audit-Complete-Since", retention.CompleteSince.Format(time.RFC3339Nano))
	response.Header().Set("X-RepoKarta-Audit-Export-Truncated", strconv.FormatBool(truncated))
	name := "repokarta-audit-" + time.Now().UTC().Format("20060102T150405Z")
	if strings.EqualFold(request.URL.Query().Get("format"), "csv") {
		response.Header().Set("Content-Type", "text/csv; charset=utf-8")
		response.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
		writer := csv.NewWriter(response)
		_ = writer.Write([]string{"id", "timestamp", "actor_id", "actor_name", "action", "target_type", "target_id", "outcome", "authentication_provider", "correlation_id", "metadata_json"})
		for _, event := range events {
			metadata, _ := json.Marshal(event.Metadata)
			_ = writer.Write([]string{
				strconv.FormatInt(event.ID, 10), event.CreatedAt.Format(time.RFC3339Nano),
				event.ActorID, event.ActorName, event.Action, event.TargetType,
				event.TargetID, event.Outcome, event.Provider, event.CorrelationID, string(metadata),
			})
		}
		writer.Flush()
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
	_ = json.NewEncoder(response).Encode(map[string]any{
		"exported_at": time.Now().UTC(), "events": events,
		"export_truncated": truncated, "retention": retention,
	})
}

func (s *Server) exportBootstrapAudit(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.security.AdminSession(request); !ok {
		http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
		return
	}
	tracker := &statusResponseWriter{ResponseWriter: response}
	s.exportAudit(tracker, request)
	outcome := "success"
	if tracker.status >= 400 {
		outcome = "failure"
	}
	s.recordAdminEvent(request, "audit.export", "audit-log", "bootstrap-export", outcome, nil)
}

func auditFilter(request *http.Request) (audit.Filter, error) {
	query := request.URL.Query()
	filter := audit.Filter{
		Query: query.Get("q"), ActorID: query.Get("actor"), Action: query.Get("action"),
		Outcome: query.Get("outcome"),
	}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return filter, errors.New("audit limit must be an integer")
		}
		filter.Limit = limit
	}
	if value := query.Get("before"); value != "" {
		before, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return filter, errors.New("audit before must be an event ID")
		}
		filter.BeforeID = before
	}
	for _, item := range []struct {
		value  string
		target *time.Time
	}{
		{query.Get("since"), &filter.Since},
		{query.Get("until"), &filter.Until},
	} {
		value, target := item.value, item.target
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return filter, errors.New("audit times must use RFC3339")
		}
		*target = parsed
	}
	return filter, nil
}

func (s *Server) updateUserRole(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.parseAdminForm(response, request, 16<<10)
	if !ok {
		return
	}
	user, err := s.enterprise.User(request.Context(), request.FormValue("user_id"))
	if err == nil {
		user.Role = identity.NormalizeRole(identity.Role(request.FormValue("role")))
		user, err = s.enterprise.SaveUser(request.Context(), user)
	}
	if err != nil {
		data := s.adminData(request.Context(), csrf)
		data.Section = "identity"
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		s.recordAdminEvent(request, "role.user.assign", "identity", request.FormValue("user_id"), "failure", nil)
		return
	}
	s.recordAdminEvent(request, "role.user.assign", "identity", user.ID, "success", map[string]string{"role": string(user.Role)})
	data := s.adminData(request.Context(), csrf)
	data.Section = "identity"
	data.Notice = "User role saved and effective for new requests."
	s.renderAdmin(response, data)
}

func (s *Server) updateGroupRole(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.parseAdminForm(response, request, 16<<10)
	if !ok {
		return
	}
	group, err := s.enterprise.Group(request.Context(), request.FormValue("group_id"))
	if err == nil {
		group.Role = identity.NormalizeRole(identity.Role(request.FormValue("role")))
		group, err = s.enterprise.SaveGroup(request.Context(), group)
	}
	if err != nil {
		data := s.adminData(request.Context(), csrf)
		data.Section = "identity"
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		s.recordAdminEvent(request, "role.group.assign", "identity-group", request.FormValue("group_id"), "failure", nil)
		return
	}
	s.recordAdminEvent(request, "role.group.assign", "identity-group", group.ID, "success", map[string]string{"role": string(group.Role)})
	data := s.adminData(request.Context(), csrf)
	data.Section = "identity"
	data.Notice = "SCIM group role saved and effective for new requests."
	s.renderAdmin(response, data)
}

func (s *Server) addRoleMapping(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.parseAdminForm(response, request, 16<<10)
	if !ok {
		return
	}
	mapping := identity.RoleMapping{
		Provider: request.FormValue("provider"), GroupValue: request.FormValue("group"),
		Role: identity.NormalizeRole(identity.Role(request.FormValue("role"))),
	}
	if err := s.enterprise.SetRoleMapping(request.Context(), mapping); err != nil {
		data := s.adminData(request.Context(), csrf)
		data.Section = "identity"
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		s.recordAdminEvent(request, "role.mapping.set", "role-mapping", mapping.Provider+":"+mapping.GroupValue, "failure", nil)
		return
	}
	s.recordAdminEvent(request, "role.mapping.set", "role-mapping", mapping.Provider+":"+mapping.GroupValue, "success", map[string]string{"role": string(mapping.Role)})
	data := s.adminData(request.Context(), csrf)
	data.Section = "identity"
	data.Notice = "Identity-provider group mapping saved and effective for new requests."
	s.renderAdmin(response, data)
}

func (s *Server) deleteRoleMapping(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.parseAdminForm(response, request, 8<<10)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(request.FormValue("mapping_id"), 10, 64)
	if err == nil {
		err = s.enterprise.DeleteRoleMapping(request.Context(), id)
	}
	if err != nil {
		data := s.adminData(request.Context(), csrf)
		data.Section = "identity"
		data.Error = "Invalid role mapping"
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
	s.recordAdminEvent(request, "role.mapping.delete", "role-mapping", strconv.FormatInt(id, 10), "success", nil)
	data := s.adminData(request.Context(), csrf)
	data.Section = "identity"
	data.Notice = "Identity-provider group mapping removed."
	s.renderAdmin(response, data)
}

func (s *Server) updateAuditRetention(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.parseAdminForm(response, request, 8<<10)
	if !ok {
		return
	}
	days, daysErr := strconv.Atoi(request.FormValue("days"))
	maxEvents, maxErr := strconv.Atoi(request.FormValue("max_events"))
	if daysErr != nil || maxErr != nil {
		data := s.adminData(request.Context(), csrf)
		data.Section = "identity"
		data.Error = "Retention values must be integers."
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
	removed, err := s.enterprise.SetAuditRetention(request.Context(), days, maxEvents)
	if err != nil {
		data := s.adminData(request.Context(), csrf)
		data.Section = "identity"
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
	s.recordAdminEvent(request, "audit.retention.update", "audit-log", "retention", "success", map[string]string{
		"days": strconv.Itoa(days), "max_events": strconv.Itoa(maxEvents),
		"removed_events": strconv.FormatInt(removed, 10),
	})
	data := s.adminData(request.Context(), csrf)
	data.Section = "identity"
	data.Notice = "Audit retention saved; removed " + strconv.FormatInt(removed, 10) + " expired events."
	s.renderAdmin(response, data)
}

func (s *Server) parseAdminForm(response http.ResponseWriter, request *http.Request, limit int64) (string, bool) {
	request.Body = http.MaxBytesReader(response, request.Body, limit)
	if err := request.ParseForm(); err != nil {
		http.Error(response, "Invalid administrator request", http.StatusBadRequest)
		return "", false
	}
	csrf, ok := s.security.AdminSession(request)
	if !ok {
		http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
		return "", false
	}
	if !s.security.ValidAdminCSRF(request, request.FormValue("csrf")) {
		http.Error(response, "Invalid administrator CSRF token", http.StatusForbidden)
		return "", false
	}
	return csrf, true
}
