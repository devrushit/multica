package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Backs the reconciler-blindness fix: ListIssues must support an
// `updated_since` RFC3339 filter and an `updated_at` sort so an external
// incremental poller (multica-sync reconciler) can fetch "issues changed since
// last tick" instead of paging the whole workspace and missing recent issues
// past the 100-row limit cap.
//
// Requires a live DB (TestMain skips the suite when none is reachable).
func TestListIssues_UpdatedSinceAndSort(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title) VALUES ($1, $2) RETURNING id
	`, testWorkspaceID, fmt.Sprintf("UpdatedSince %d", suffix)).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	// Seed three issues, then stamp deterministic updated_at values:
	//   old   → 2000-01-01
	//   mid   → 2020-06-15
	//   fresh → 2040-12-31
	insertIssue := func(title string, updatedAt time.Time) string {
		var number int
		if err := testPool.QueryRow(ctx, `
			UPDATE workspace
			SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
			WHERE id = $1 RETURNING issue_counter
		`, testWorkspaceID).Scan(&number); err != nil {
			t.Fatalf("next issue number: %v", err)
		}
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, position, number, project_id)
			VALUES ($1, $2, 'todo', 'none', 'member', $3, 0, $4, $5) RETURNING id
		`, testWorkspaceID, title, testUserID, number, projectID).Scan(&id); err != nil {
			t.Fatalf("create issue %q: %v", title, err)
		}
		if _, err := testPool.Exec(ctx, `UPDATE issue SET updated_at = $1 WHERE id = $2`, updatedAt, id); err != nil {
			t.Fatalf("stamp updated_at %q: %v", title, err)
		}
		return id
	}

	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2020, 6, 15, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2040, 12, 31, 0, 0, 0, 0, time.UTC)
	_ = insertIssue(fmt.Sprintf("us-old-%d", suffix), old)
	idMid := insertIssue(fmt.Sprintf("us-mid-%d", suffix), mid)
	idFresh := insertIssue(fmt.Sprintf("us-fresh-%d", suffix), fresh)

	type listResp struct {
		Issues []IssueResponse `json:"issues"`
		Total  int64           `json:"total"`
	}
	call := func(query string) (int, listResp, string) {
		path := fmt.Sprintf("/api/issues?workspace_id=%s&project_id=%s%s", testWorkspaceID, projectID, query)
		w := httptest.NewRecorder()
		testHandler.ListIssues(w, newRequest("GET", path, nil))
		var resp listResp
		body := w.Body.String()
		if w.Code == http.StatusOK {
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode (q=%q): %v\nbody: %s", query, err, body)
			}
		}
		return w.Code, resp, body
	}

	t.Run("updated_since cutoff between mid and fresh returns only fresh", func(t *testing.T) {
		cutoff := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		code, resp, body := call("&updated_since=" + cutoff)
		if code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", code, body)
		}
		if resp.Total != 1 || len(resp.Issues) != 1 {
			t.Fatalf("want 1 issue (fresh), got total=%d len=%d", resp.Total, len(resp.Issues))
		}
		if resp.Issues[0].ID != idFresh {
			t.Fatalf("want fresh issue %s, got %s", idFresh, resp.Issues[0].ID)
		}
	})

	t.Run("updated_since in the far past returns all three", func(t *testing.T) {
		cutoff := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		code, resp, body := call("&updated_since=" + cutoff)
		if code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", code, body)
		}
		if resp.Total != 3 {
			t.Fatalf("want 3, got %d", resp.Total)
		}
	})

	t.Run("updated_since inclusive of exact boundary", func(t *testing.T) {
		code, resp, body := call("&updated_since=" + mid.Format(time.RFC3339))
		if code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", code, body)
		}
		// >= mid → mid and fresh
		if resp.Total != 2 {
			t.Fatalf("want 2 (mid+fresh, inclusive), got %d", resp.Total)
		}
	})

	t.Run("invalid updated_since is 400", func(t *testing.T) {
		code, _, _ := call("&updated_since=not-a-date")
		if code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", code)
		}
	})

	t.Run("sort=updated_at desc orders newest first", func(t *testing.T) {
		code, resp, body := call("&sort=updated_at&direction=desc")
		if code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", code, body)
		}
		if len(resp.Issues) != 3 {
			t.Fatalf("want 3, got %d", len(resp.Issues))
		}
		// desc by updated_at → fresh(2040), mid(2020), old(2000).
		if resp.Issues[0].ID != idFresh {
			t.Fatalf("desc head: want fresh %s, got %s", idFresh, resp.Issues[0].ID)
		}
		if resp.Issues[1].ID != idMid {
			t.Fatalf("desc[1]: want mid %s, got %s", idMid, resp.Issues[1].ID)
		}
	})

	t.Run("sort=updated_at asc orders oldest first", func(t *testing.T) {
		code, resp, body := call("&sort=updated_at&direction=asc")
		if code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", code, body)
		}
		if len(resp.Issues) != 3 {
			t.Fatalf("want 3, got %d", len(resp.Issues))
		}
		if resp.Issues[2].ID != idFresh {
			t.Fatalf("asc tail: want fresh %s, got %s", idFresh, resp.Issues[2].ID)
		}
	})
}
