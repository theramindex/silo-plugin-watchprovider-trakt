package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const maxEpisodeResolutionCacheEntries = 2048

type traktShowSearchCandidate struct {
	namespace string
	value     string
}

type traktShowSearchResult struct {
	Type string    `json:"type"`
	Show traktShow `json:"show"`
}

// applyScrobble forwards one playback event. Trakt itself decides from the
// progress whether a stop records a play (at 80% or more), as it did for
// Silo's built-in provider.
func (s *Server) applyScrobble(ctx context.Context, client *apiClient, operation pluginv1.WatchSyncOperation, group eventGroup) *pluginv1.WatchSyncFault {
	index := group.pending[0]
	event := group.events[index]
	if kind := mediaKind(event.GetMedia()); kind != kindMovie && kind != kindEpisode {
		group.reject(index, "Trakt scrobbles movies and episodes only")
		return nil
	}

	payload, fault := s.scrobblePayload(ctx, client, event, s.clock())
	if fault != nil {
		return group.fail([]int{index}, fault)
	}

	path := "/scrobble/start"
	switch operation {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE:
		path = "/scrobble/pause"
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP:
		path = "/scrobble/stop"
	}
	status, _, fault := client.do(ctx, http.MethodPost, path, nil, payload, nil)
	if fault != nil {
		// Trakt answers 409 when it scrobbled the same item moments ago, so a
		// redelivered stop does not record a second play.
		if status == http.StatusConflict {
			group.set(index, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
			return nil
		}
		return group.fail([]int{index}, fault)
	}
	group.set(index, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
	return nil
}

func (s *Server) scrobblePayload(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent, now time.Time) (map[string]any, *pluginv1.WatchSyncFault) {
	progress := 0.0
	if event.GetDurationSeconds() > 0 {
		progress = event.GetPositionSeconds() / event.GetDurationSeconds() * 100
	}
	payload := map[string]any{
		"progress":    progress,
		"app_version": "Silo",
		"app_date":    now.UTC().Format("2006-01-02"),
	}

	media := event.GetMedia()
	switch mediaKind(media) {
	case kindMovie:
		payload["movie"] = map[string]any{"ids": traktIDsFrom(media.GetExternalIds())}
	case kindEpisode:
		ids, fault := s.episodeScrobbleIDs(ctx, client, media)
		if fault != nil {
			return nil, fault
		}
		// The current Trakt scrobble contract accepts only a Trakt or TVDB
		// episode ID here. IMDb/TMDb episode IDs and the old show + season +
		// number payload are not valid scrobble identifiers.
		payload["episode"] = map[string]any{"ids": ids}
	default:
		return nil, invalidRequestFault("Trakt scrobbles movies and episodes only")
	}
	return payload, nil
}

// episodeScrobbleIDs returns an identifier accepted by Trakt's current
// episode-scrobble schema. When Silo supplies a parent series and coordinates,
// resolve the episode through Trakt instead of trusting possibly stale or
// series-level IDs carried in the episode's own external_ids. If the event
// lacks enough data for that lookup, a TVDB episode ID is the only supported
// direct fallback. An ambiguous or contradictory lookup is rejected rather
// than risking a scrobble against the wrong episode.
func (s *Server) episodeScrobbleIDs(ctx context.Context, client *apiClient, media *pluginv1.WatchSyncMedia) (map[string]any, *pluginv1.WatchSyncFault) {
	seriesIDs := media.GetSeriesExternalIds()
	season, episode := int(media.GetSeasonNumber()), int(media.GetEpisodeNumber())
	candidates := traktShowSearchCandidates(seriesIDs)

	if len(candidates) > 0 && episode > 0 {
		cacheKey := episodeResolutionCacheKey(seriesIDs, season, episode)
		if id, ok := s.cachedEpisodeTraktID(cacheKey); ok {
			return map[string]any{"trakt": id}, nil
		}

		showID, fault := s.resolveTraktShowID(ctx, client, candidates)
		if fault != nil {
			return nil, fault
		}

		var resolved traktEpisode
		path := fmt.Sprintf("/shows/%d/seasons/%d/episodes/%d", showID, season, episode)
		status, _, fault := client.do(ctx, http.MethodGet, path, nil, nil, &resolved)
		if fault != nil {
			if status == http.StatusNotFound {
				return nil, invalidRequestFault("Trakt could not resolve the requested season and episode; scrobble was not sent")
			}
			return nil, fault
		}
		if int(resolved.Season) != season || int(resolved.Number) != episode {
			return nil, invalidRequestFault("Trakt returned an episode that does not match Silo's season and episode; scrobble was not sent")
		}
		if resolved.IDs.Trakt > 0 {
			s.cacheEpisodeTraktID(cacheKey, resolved.IDs.Trakt)
			return map[string]any{"trakt": resolved.IDs.Trakt}, nil
		}
		if resolved.IDs.TVDB > 0 {
			return map[string]any{"tvdb": resolved.IDs.TVDB}, nil
		}
		return nil, invalidRequestFault("Trakt did not return a usable episode ID; scrobble was not sent")
	}

	// Do not send IMDb/TMDb IDs as episode IDs: Trakt's current scrobble
	// schema does not accept them. TVDB episode IDs are supported directly.
	if tvdbID := parseInt(strings.TrimSpace(media.GetExternalIds()["tvdb"])); tvdbID > 0 {
		return map[string]any{"tvdb": tvdbID}, nil
	}
	return nil, invalidRequestFault("Trakt cannot safely identify this episode; a valid TVDB episode ID or series ID with season and episode numbers is required")
}

func traktShowSearchCandidates(ids map[string]string) []traktShowSearchCandidate {
	candidates := make([]traktShowSearchCandidate, 0, 3)
	// Prefer TVDB, which is commonly available for TV metadata. Search results
	// are validated against the queried namespace before their Trakt ID is used.
	if value := parseInt(strings.TrimSpace(ids["tvdb"])); value > 0 {
		candidates = append(candidates, traktShowSearchCandidate{namespace: "tvdb", value: fmt.Sprint(value)})
	}
	if value := strings.TrimSpace(ids["imdb"]); value != "" {
		candidates = append(candidates, traktShowSearchCandidate{namespace: "imdb", value: value})
	}
	if value := parseInt(strings.TrimSpace(ids["tmdb"])); value > 0 {
		candidates = append(candidates, traktShowSearchCandidate{namespace: "tmdb", value: fmt.Sprint(value)})
	}
	return candidates
}

func (s *Server) resolveTraktShowID(ctx context.Context, client *apiClient, candidates []traktShowSearchCandidate) (int, *pluginv1.WatchSyncFault) {
	for _, candidate := range candidates {
		query := url.Values{"type": []string{"show"}}
		var results []traktShowSearchResult
		path := fmt.Sprintf("/search/%s/%s", candidate.namespace, url.PathEscape(candidate.value))
		status, _, fault := client.do(ctx, http.MethodGet, path, query, nil, &results)
		if fault != nil {
			if status == http.StatusNotFound {
				continue
			}
			return 0, fault
		}

		matchedID := 0
		for _, result := range results {
			if result.Type != "show" || !traktShowMatchesSearchCandidate(result.Show.IDs, candidate) || result.Show.IDs.Trakt <= 0 {
				continue
			}
			if matchedID != 0 && matchedID != result.Show.IDs.Trakt {
				return 0, invalidRequestFault("Trakt returned an ambiguous series identity; scrobble was not sent")
			}
			matchedID = result.Show.IDs.Trakt
		}
		if matchedID > 0 {
			return matchedID, nil
		}
	}
	return 0, invalidRequestFault("Trakt could not verify the series identity for this episode; scrobble was not sent")
}

func traktShowMatchesSearchCandidate(ids traktIDs, candidate traktShowSearchCandidate) bool {
	switch candidate.namespace {
	case "imdb":
		return ids.IMDb == candidate.value
	case "tmdb":
		return ids.TMDB == parseInt(candidate.value)
	case "tvdb":
		return ids.TVDB == parseInt(candidate.value)
	default:
		return false
	}
}

func episodeResolutionCacheKey(seriesIDs map[string]string, season, episode int) string {
	// Use an explicit field order so map iteration cannot create duplicate keys.
	return fmt.Sprintf("tvdb=%q|imdb=%q|tmdb=%q|season=%d|episode=%d",
		strings.TrimSpace(seriesIDs["tvdb"]),
		strings.TrimSpace(seriesIDs["imdb"]),
		strings.TrimSpace(seriesIDs["tmdb"]),
		season, episode)
}

func (s *Server) cachedEpisodeTraktID(key string) (int, bool) {
	s.episodeIDMu.RLock()
	defer s.episodeIDMu.RUnlock()
	id, ok := s.episodeIDCache[key]
	return id, ok && id > 0
}

func (s *Server) cacheEpisodeTraktID(key string, id int) {
	if id <= 0 {
		return
	}
	s.episodeIDMu.Lock()
	defer s.episodeIDMu.Unlock()
	if s.episodeIDCache == nil {
		s.episodeIDCache = make(map[string]int)
	}
	if len(s.episodeIDCache) >= maxEpisodeResolutionCacheEntries {
		// Keep cache memory bounded for long-running servers.
		clear(s.episodeIDCache)
	}
	s.episodeIDCache[key] = id
}
