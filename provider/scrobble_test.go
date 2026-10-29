package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func episodeScrobbleEvent(id string, operation pluginv1.WatchSyncOperation) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:         id,
		Operation:       operation,
		PositionSeconds: 85,
		DurationSeconds: 100,
		Media: &pluginv1.WatchSyncMedia{
			MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			Title:     "The episode that is playing",
			ExternalIds: map[string]string{
				"imdb": "tt-episode",
				"tmdb": "9999",
				"tvdb": "8888",
			},
			SeriesTitle: "The correct series",
			SeriesExternalIds: map[string]string{
				"tvdb": "12345",
				"tmdb": "4567",
			},
			SeasonNumber:  2,
			EpisodeNumber: 7,
		},
	}
}

func TestScrobbleResolvesCanonicalEpisodeIDAndCachesPlaybackLifecycle(t *testing.T) {
	t.Parallel()

	requestCounts := map[string]int{}
	payloads := make([]map[string]any, 0, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCounts[r.Method+" "+r.URL.Path]++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/search/tvdb/12345":
			if r.URL.Query().Get("type") != "show" {
				t.Errorf("show lookup type = %q, want show", r.URL.Query().Get("type"))
			}
			writeFixture(t, w, `[{"type":"show","score":100,"show":{"title":"The correct series","year":2020,"ids":{"trakt":321,"tvdb":12345,"tmdb":4567,"imdb":"tt-series"}}}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/shows/321/seasons/2/episodes/7":
			writeFixture(t, w, `{"title":"The episode that is playing","season":2,"number":7,"ids":{"trakt":987,"tvdb":777,"tmdb":333,"imdb":"tt-episode"}}`)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/scrobble/"):
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode scrobble request: %v", err)
			}
			payloads = append(payloads, payload)
			writeFixture(t, w, `{}`)
		default:
			t.Errorf("unexpected Trakt request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	server, _ := newTestServer(t, upstream)
	server.writes = newCredentialLimiter(time.Nanosecond, 10)
	events := []*pluginv1.WatchSyncEvent{
		episodeScrobbleEvent("start", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START),
		episodeScrobbleEvent("pause", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE),
		episodeScrobbleEvent("stop", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP),
	}
	got := statuses(t, applyEvents(t, server, events...), events...)
	for id, status := range got {
		if status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
			t.Errorf("%s status = %s, want APPLIED", id, status)
		}
	}

	if requestCounts["GET /search/tvdb/12345"] != 1 || requestCounts["GET /shows/321/seasons/2/episodes/7"] != 1 {
		t.Errorf("episode lookup requests = %v, want one series search and one episode lookup", requestCounts)
	}
	if len(payloads) != 3 {
		t.Fatalf("scrobble requests = %d, want 3", len(payloads))
	}
	for i, payload := range payloads {
		episode, ok := payload["episode"].(map[string]any)
		if !ok {
			t.Fatalf("payload %d has no episode object: %v", i, payload)
		}
		ids, ok := episode["ids"].(map[string]any)
		if !ok || len(ids) != 1 || ids["trakt"] != float64(987) {
			t.Errorf("payload %d episode IDs = %v, want only canonical Trakt episode ID 987", i, episode["ids"])
		}
		if _, ok := episode["season"]; ok {
			t.Errorf("payload %d still uses obsolete episode season field: %v", i, episode)
		}
		if _, ok := payload["show"]; ok {
			t.Errorf("payload %d still uses obsolete show fallback: %v", i, payload)
		}
	}
}

func TestScrobbleRejectsMismatchedEpisodeLookup(t *testing.T) {
	t.Parallel()

	var scrobbleRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/tvdb/12345":
			writeFixture(t, w, `[{"type":"show","show":{"ids":{"trakt":321,"tvdb":12345}}}]`)
		case "/shows/321/seasons/2/episodes/7":
			// Trakt has returned a different episode than the requested S2E7.
			writeFixture(t, w, `{"title":"Wrong episode","season":2,"number":1,"ids":{"trakt":101,"tvdb":202}}`)
		default:
			if strings.HasPrefix(r.URL.Path, "/scrobble/") {
				scrobbleRequests++
			}
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	server, _ := newTestServer(t, upstream)
	event := episodeScrobbleEvent("mismatch", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START)
	response := applyEvents(t, server, event)
	got := statuses(t, response, event)
	if got[event.GetEventId()] != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("status = %s, want REJECTED", got[event.GetEventId()])
	}
	if !strings.Contains(response.GetResults()[0].GetFault().GetSafeMessage(), "does not match") {
		t.Errorf("fault = %v, want mismatch diagnostic", response.GetResults()[0].GetFault())
	}
	if scrobbleRequests != 0 {
		t.Errorf("sent %d scrobble requests for a mismatched lookup", scrobbleRequests)
	}
}

func TestScrobbleUsesTVDBEpisodeIDWhenSeriesLookupIsUnavailable(t *testing.T) {
	t.Parallel()

	var body map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/scrobble/start" {
			t.Errorf("unexpected Trakt request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode scrobble request: %v", err)
		}
		writeFixture(t, w, `{}`)
	}))
	defer upstream.Close()

	server, _ := newTestServer(t, upstream)
	event := episodeScrobbleEvent("tvdb-direct", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START)
	event.Media.SeriesExternalIds = nil
	event.Media.SeasonNumber = 0
	event.Media.EpisodeNumber = 0
	event.Media.ExternalIds = map[string]string{"tvdb": "456"}
	response := applyEvents(t, server, event)
	if status := statuses(t, response, event)[event.GetEventId()]; status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("status = %s, want APPLIED", status)
	}
	episode, ok := body["episode"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no episode object: %v", body)
	}
	ids, ok := episode["ids"].(map[string]any)
	if !ok || len(ids) != 1 || ids["tvdb"] != float64(456) {
		t.Fatalf("episode IDs = %v, want only TVDB episode ID 456", episode["ids"])
	}
}

func TestScrobbleRejectsUnsupportedEpisodeIDsWithoutSeriesIdentity(t *testing.T) {
	t.Parallel()

	var requests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	server, _ := newTestServer(t, upstream)
	event := episodeScrobbleEvent("no-supported-id", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START)
	event.Media.SeriesExternalIds = nil
	event.Media.SeasonNumber = 0
	event.Media.EpisodeNumber = 0
	event.Media.ExternalIds = map[string]string{"imdb": "tt-episode", "tmdb": "9999"}
	response := applyEvents(t, server, event)
	if status := statuses(t, response, event)[event.GetEventId()]; status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("status = %s, want REJECTED", status)
	}
	if requests != 0 {
		t.Errorf("made %d Trakt requests for an episode without a supported identity", requests)
	}
}
