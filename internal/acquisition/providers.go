package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	maximumDiscoveryPages      = 10
	maximumRepositoriesPerPage = 100
	maximumProviderResponse    = 8 << 20
)

func (s *Service) discoverLocal(request DiscoverRequest) ([]Candidate, error) {
	repositories, err := catalog.Discover(request.Location)
	if err != nil {
		return nil, err
	}
	candidates := make([]Candidate, 0, len(repositories))
	for _, repository := range repositories {
		canonicalID := localCanonicalID(repository.Path)
		namespace := strings.TrimSuffix(strings.TrimSpace(repository.OriginURL), ".git")
		candidates = append(candidates, Candidate{
			Provider:      ProviderLocal,
			CanonicalID:   canonicalID,
			Name:          repository.Name,
			Namespace:     namespace,
			RemoteURL:     repository.OriginURL,
			LocalPath:     repository.Path,
			DefaultBranch: repository.DefaultRevision,
			Visibility:    "local",
		})
	}
	return candidates, nil
}

type githubRepository struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	CloneURL      string   `json:"clone_url"`
	HTMLURL       string   `json:"html_url"`
	DefaultBranch string   `json:"default_branch"`
	Visibility    string   `json:"visibility"`
	Private       bool     `json:"private"`
	Archived      bool     `json:"archived"`
	Fork          bool     `json:"fork"`
	Topics        []string `json:"topics"`
}

func (s *Service) discoverGitHub(ctx context.Context, request DiscoverRequest) ([]Candidate, error) {
	location, direct, err := providerLocation(request.Location, s.githubHost)
	if err != nil {
		return nil, err
	}
	var repositories []githubRepository
	if direct {
		var repository githubRepository
		if err := s.providerJSON(ctx, ProviderGitHub, request.CredentialRef,
			s.githubAPI+"/repos/"+pathEscapeSegments(location), &repository); err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	} else {
		repositories, err = s.githubNamespace(ctx, request.CredentialRef, location, request.Team)
		if err != nil {
			return nil, err
		}
	}
	candidates := make([]Candidate, 0, len(repositories))
	for _, repository := range repositories {
		visibility := strings.TrimSpace(repository.Visibility)
		if visibility == "" {
			if repository.Private {
				visibility = "private"
			} else {
				visibility = "public"
			}
		}
		candidate := Candidate{
			Provider:             ProviderGitHub,
			ProviderRepositoryID: providerRepositoryID(repository.ID),
			CanonicalID:          strings.ToLower(s.githubHost + "/" + repository.FullName),
			Name:                 repository.Name,
			Namespace:            strings.TrimSuffix(repository.FullName, "/"+repository.Name),
			RemoteURL:            repository.CloneURL,
			WebURL:               repository.HTMLURL,
			DefaultBranch:        repository.DefaultBranch,
			Visibility:           visibility,
			Topics:               append([]string(nil), repository.Topics...),
			Archived:             repository.Archived,
			Forked:               repository.Fork,
		}
		applyDiscoveryPolicy(&candidate, request)
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func (s *Service) githubNamespace(ctx context.Context, credentialRef, namespace, team string) ([]githubRepository, error) {
	var repositories []githubRepository
	endpointKind := "org"
	if strings.TrimSpace(team) != "" {
		endpointKind = "team"
	}
	for pageNumber := 1; pageNumber <= maximumDiscoveryPages; pageNumber++ {
		var pageRepositories []githubRepository
		var endpoint string
		if endpointKind == "team" {
			endpoint = fmt.Sprintf("%s/orgs/%s/teams/%s/repos?per_page=%d&page=%d",
				s.githubAPI, url.PathEscape(namespace), url.PathEscape(strings.TrimSpace(team)),
				maximumRepositoriesPerPage, pageNumber)
		} else if endpointKind == "user" {
			endpoint = fmt.Sprintf("%s/users/%s/repos?type=owner&per_page=%d&page=%d",
				s.githubAPI, url.PathEscape(namespace), maximumRepositoriesPerPage, pageNumber)
		} else {
			endpoint = fmt.Sprintf("%s/orgs/%s/repos?type=all&per_page=%d&page=%d",
				s.githubAPI, url.PathEscape(namespace), maximumRepositoriesPerPage, pageNumber)
		}
		err := s.providerJSON(ctx, ProviderGitHub, credentialRef, endpoint, &pageRepositories)
		if pageNumber == 1 && endpointKind == "org" && errors.Is(err, errProviderNotFound) {
			endpointKind = "user"
			endpoint = fmt.Sprintf("%s/users/%s/repos?type=owner&per_page=%d&page=%d",
				s.githubAPI, url.PathEscape(namespace), maximumRepositoriesPerPage, pageNumber)
			err = s.providerJSON(ctx, ProviderGitHub, credentialRef, endpoint, &pageRepositories)
		}
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, pageRepositories...)
		if len(pageRepositories) < maximumRepositoriesPerPage {
			return repositories, nil
		}
	}
	return repositories, nil
}

type gitlabRepository struct {
	ID                  int64    `json:"id"`
	Name                string   `json:"name"`
	PathWithNamespace   string   `json:"path_with_namespace"`
	HTTPURLToRepository string   `json:"http_url_to_repo"`
	WebURL              string   `json:"web_url"`
	DefaultBranch       string   `json:"default_branch"`
	Visibility          string   `json:"visibility"`
	Archived            bool     `json:"archived"`
	ForkedFromProject   any      `json:"forked_from_project"`
	Topics              []string `json:"topics"`
}

func (s *Service) discoverGitLab(ctx context.Context, request DiscoverRequest) ([]Candidate, error) {
	location, direct, err := providerLocation(request.Location, s.gitlabHost)
	if err != nil {
		return nil, err
	}
	var repositories []gitlabRepository
	if direct {
		var repository gitlabRepository
		err = s.providerJSON(ctx, ProviderGitLab, request.CredentialRef,
			s.gitlabAPI+"/projects/"+url.PathEscape(location), &repository)
		if err == nil {
			repositories = append(repositories, repository)
		} else if !errors.Is(err, errProviderNotFound) {
			return nil, err
		} else {
			direct = false
		}
	}
	if !direct {
		for pageNumber := 1; pageNumber <= maximumDiscoveryPages; pageNumber++ {
			var pageRepositories []gitlabRepository
			endpoint := fmt.Sprintf("%s/groups/%s/projects?include_subgroups=true&per_page=%d&page=%d",
				s.gitlabAPI, url.PathEscape(location), maximumRepositoriesPerPage, pageNumber)
			if err := s.providerJSON(ctx, ProviderGitLab, request.CredentialRef, endpoint, &pageRepositories); err != nil {
				return nil, err
			}
			repositories = append(repositories, pageRepositories...)
			if len(pageRepositories) < maximumRepositoriesPerPage {
				break
			}
		}
	}
	candidates := make([]Candidate, 0, len(repositories))
	for _, repository := range repositories {
		candidate := Candidate{
			Provider:             ProviderGitLab,
			ProviderRepositoryID: providerRepositoryID(repository.ID),
			CanonicalID:          strings.ToLower(s.gitlabHost + "/" + repository.PathWithNamespace),
			Name:                 repository.Name,
			Namespace:            strings.TrimSuffix(repository.PathWithNamespace, "/"+repository.Name),
			RemoteURL:            repository.HTTPURLToRepository,
			WebURL:               repository.WebURL,
			DefaultBranch:        repository.DefaultBranch,
			Visibility:           repository.Visibility,
			Topics:               append([]string(nil), repository.Topics...),
			Archived:             repository.Archived,
			Forked:               repository.ForkedFromProject != nil,
		}
		applyDiscoveryPolicy(&candidate, request)
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func applyDiscoveryPolicy(candidate *Candidate, request DiscoverRequest) {
	switch {
	case candidate.Archived && !request.IncludeArchived:
		candidate.Excluded = true
		candidate.Exclusion = "archived repositories are excluded by this preview"
	case candidate.Forked && !request.IncludeForks:
		candidate.Excluded = true
		candidate.Exclusion = "forks are excluded by this preview"
	case candidate.Visibility != "public" && !request.IncludePrivate:
		candidate.Excluded = true
		candidate.Exclusion = "non-public repositories are excluded by this preview"
	case !containsTopics(candidate.Topics, request.Topics):
		candidate.Excluded = true
		candidate.Exclusion = "repository does not match every required topic"
	case len(request.Allow) > 0 && !matchesAny(candidate, request.Allow):
		candidate.Excluded = true
		candidate.Exclusion = "repository does not match the allow policy"
	case matchesAny(candidate, request.Deny):
		candidate.Excluded = true
		candidate.Exclusion = "repository matches the deny policy"
	}
}

func containsTopics(candidateTopics, required []string) bool {
	available := make(map[string]struct{}, len(candidateTopics))
	for _, topic := range candidateTopics {
		available[strings.ToLower(strings.TrimSpace(topic))] = struct{}{}
	}
	for _, topic := range required {
		if _, ok := available[strings.ToLower(strings.TrimSpace(topic))]; !ok {
			return false
		}
	}
	return true
}

func matchesAny(candidate *Candidate, patterns []string) bool {
	identity := strings.Trim(candidate.Namespace+"/"+candidate.Name, "/")
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if matched, _ := path.Match(pattern, identity); matched {
			return true
		}
		if matched, _ := path.Match(pattern, candidate.Name); matched {
			return true
		}
	}
	return false
}

func providerRepositoryID(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

var errProviderNotFound = errors.New("provider resource not found")

func (s *Service) providerJSON(ctx context.Context, provider, credentialRef, endpoint string, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "RepoKarta/"+s.version)
	if credentialRef != "" {
		token := strings.TrimSpace(os.Getenv(credentialRef))
		if token == "" {
			return fmt.Errorf("credential reference %q is not available in the RepoKarta environment", credentialRef)
		}
		if provider == ProviderGitHub {
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		} else {
			request.Header.Set("PRIVATE-TOKEN", token)
		}
	}
	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s discovery request: %w", provider, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return errProviderNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden {
			retry := strings.TrimSpace(response.Header.Get("Retry-After"))
			if reset, err := strconv.ParseInt(strings.TrimSpace(response.Header.Get("X-RateLimit-Reset")), 10, 64); err == nil && reset > 0 {
				retry = time.Unix(reset, 0).UTC().Format(time.RFC3339)
			}
			if retry != "" {
				return fmt.Errorf("%s discovery rate limited; retry after %s", provider, retry)
			}
		}
		return fmt.Errorf("%s discovery failed with HTTP %d", provider, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maximumProviderResponse))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode %s discovery response: %w", provider, err)
	}
	return nil
}

func providerLocation(value, expectedHost string) (string, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false, errors.New("repository location is required")
	}
	direct := false
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		if !strings.EqualFold(parsed.Host, expectedHost) {
			return "", false, fmt.Errorf("expected a %s URL", expectedHost)
		}
		value = strings.Trim(parsed.Path, "/")
		direct = strings.Count(value, "/") >= 1
	} else {
		value = strings.Trim(value, "/")
		direct = strings.Count(value, "/") >= 1
	}
	value = strings.TrimSuffix(value, ".git")
	if value == "" || strings.Contains(value, "..") {
		return "", false, errors.New("repository namespace is invalid")
	}
	return value, direct, nil
}

func pathEscapeSegments(value string) string {
	segments := strings.Split(value, "/")
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	return strings.Join(segments, "/")
}

func parseRetryAfter(value string) int {
	seconds, _ := strconv.Atoi(strings.TrimSpace(value))
	return seconds
}
