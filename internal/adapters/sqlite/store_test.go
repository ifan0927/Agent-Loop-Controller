package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestGitHubV6EvidencePersistsMetadataWithoutSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	input := application.CreateRunInput{Run: application.Run{ID: "run-gh", IssueID: "IFAN-GH", IdempotencyKey: "key-gh", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "feature", ArtifactRoot: "/tmp/run-gh", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	if _, _, err := store.CreateRun(ctx, input); err != nil {
		t.Fatal(err)
	}
	repo := domain.RepositoryIdentity{ID: 99, NodeID: "REPO", Owner: "owner", Name: "repo"}
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	if err := store.SaveGitHubInstallation(ctx, "run-gh", application.GitHubInstallationMetadata{AppID: 1, InstallationID: 2, Repository: repo, TokenExpiresAt: now.Add(time.Hour), PermissionsDigest: "digest", ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGitHubRequest(ctx, application.GitHubRequestObservation{RunID: "run-gh", Operation: "repository", Category: "REST", HTTPStatus: 200, ResponseDigest: "response-digest", InstallationID: 2, Repository: repo, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	e := domain.GitHubReadEvidence{Repository: repo, PullRequest: domain.PullRequest{Number: 1, HeadSHA: "head", BaseSHA: "base"}, ObservedAt: now}
	if err := store.SaveGitHubEvidence(ctx, "run-gh", e); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, "run-gh")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.GitHubInstallation == nil || len(inspection.GitHubRequests) != 1 || inspection.GitHubEvidence == nil {
		t.Fatalf("missing GitHub v6 inspection: %+v", inspection)
	}
	store.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("fixture-installation-secret"), []byte("BEGIN PRIVATE KEY"), []byte("Bearer "), []byte("eyJhbGci")} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("database contains secret marker %q", secret)
		}
	}
}

func TestMigrationAndRunIdempotency(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key-1", SourceRevision: "v1",
		RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run-1", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	if _, created, err := store.CreateRun(context.Background(), input); err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if _, created, err := store.CreateRun(context.Background(), input); err != nil || created {
		t.Fatalf("repeat: created=%v err=%v", created, err)
	}
	other := input
	other.Run.ID = "run-2"
	other.Run.IdempotencyKey = "key-2"
	if _, _, err := store.CreateRun(context.Background(), other); err == nil {
		t.Fatal("active issue uniqueness must reject second run")
	}
}

func TestDatabaseIsPrivateAndMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode=%o", info.Mode().Perm())
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestMigratesVersionOneDatabaseToVersionTwo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range migrationV1 {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(1,'v1')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name IN ('lease_owner','lease_expires_unix','implementation_model','review_model')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatal("current run lease or model columns are missing")
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('attempts') WHERE name='requested_model'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("attempt requested_model column missing: count=%d err=%v", count, err)
	}
}

func TestMigratesVersionFourDatabaseToCurrentVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for version, migration := range [][]string{migrationV1, migrationV2, migrationV3, migrationV4} {
		for _, statement := range migration {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("v%d: %v", version+1, err)
			}
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, version+1, fmt.Sprintf("v%d", version+1)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != 6 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('side_effects','pull_requests','poll_observations','review_findings','human_approvals','merge_results','cleanup_results')`).Scan(&count); err != nil || count != 7 {
		t.Fatalf("delivery tables=%d err=%v", count, err)
	}
}

func TestSideEffectIntentSurvivesRestartWithoutDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/one", ArtifactRoot: "/tmp/run", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	intent := application.SideEffectRecord{RunID: "run-1", Kind: "push", IdempotencyKey: "h1", IntentJSON: `{"head":"h1"}`, Attempt: 1}
	first, created, err := store.BeginSideEffect(context.Background(), intent)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	store.Close()
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, created, err := store.BeginSideEffect(context.Background(), intent)
	if err != nil || created || second.ID != first.ID || second.Status != "intent" {
		t.Fatalf("first=%+v second=%+v created=%v err=%v", first, second, created, err)
	}
}

func TestApprovalAndMergeEvidenceAreImmutable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/one", ArtifactRoot: "/tmp/run", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	if _, _, err := store.CreateRun(ctx, input); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Nanosecond)
	approval := domain.HumanApproval{PRNumber: 1, Approver: "ifan0927", Source: "github_review", ApprovedSHA: "h1", CIStatus: "pass", CodeRabbit: "pass", ReviewSHA: "h1", ApprovedAt: now}
	if err := store.SaveHumanApproval(ctx, "run-1", approval); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveHumanApproval(ctx, "run-1", approval); err != nil {
		t.Fatal(err)
	}
	changed := approval
	changed.ApprovedSHA = "h2"
	changed.ReviewSHA = "h2"
	if err := store.SaveHumanApproval(ctx, "run-1", changed); err != nil {
		t.Fatalf("new HEAD approval: %v", err)
	}
	conflict := changed
	conflict.Approver = "mallory"
	if err := store.SaveHumanApproval(ctx, "run-1", conflict); err == nil {
		t.Fatal("conflicting same-HEAD approval must fail closed")
	}
	if err := store.SetCandidateHead(ctx, "run-1", "h2"); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, "run-1")
	if err != nil || inspection.Approval == nil || inspection.Approval.ApprovedSHA != "h2" {
		t.Fatalf("current approval=%+v err=%v", inspection.Approval, err)
	}
	merge := application.MergeRecord{RunID: "run-1", PRNumber: 1, PreMergeSHA: "h1", BaseSHA: "b1", Method: "squash", MergeSHA: "m1", MergedAt: now}
	if err := store.SaveMerge(ctx, merge); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMerge(ctx, merge); err != nil {
		t.Fatal(err)
	}
	changedMerge := merge
	changedMerge.MergeSHA = "m2"
	if err := store.SaveMerge(ctx, changedMerge); err == nil {
		t.Fatal("conflicting merge must fail closed")
	}
}

func TestSideEffectAndPullRequestConflictsFailClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/one", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(ctx, input); err != nil {
		t.Fatal(err)
	}
	intent := application.SideEffectRecord{RunID: "run-1", Kind: "push", IdempotencyKey: "h1", IntentJSON: `{"head":"h1"}`, Attempt: 1}
	if _, _, err := store.BeginSideEffect(ctx, intent); err != nil {
		t.Fatal(err)
	}
	changed := intent
	changed.IntentJSON = `{"head":"other"}`
	if _, _, err := store.BeginSideEffect(ctx, changed); err == nil {
		t.Fatal("conflicting side-effect intent must fail")
	}
	pr := domain.PullRequest{Number: 1, URL: "https://fixture/1", NodeID: "n1", HeadBranch: "ifan/one", BaseBranch: "main", HeadSHA: "h1", BaseSHA: "b1", BodyDigest: "d1", OwnershipKey: "key", State: "OPEN"}
	if err := store.SavePullRequest(ctx, "run-1", pr); err != nil {
		t.Fatal(err)
	}
	changedPR := pr
	changedPR.BaseBranch = "other"
	if err := store.SavePullRequest(ctx, "run-1", changedPR); err == nil {
		t.Fatal("conflicting PR evidence must fail")
	}
	updatedPR := pr
	updatedPR.HeadSHA = "h2"
	updatedPR.BodyDigest = "d2"
	if err := store.SavePullRequest(ctx, "run-1", updatedPR); err != nil {
		t.Fatalf("owned PR head update: %v", err)
	}
	inspection, err := store.Inspect(ctx, "run-1")
	if err != nil || inspection.PullRequest == nil || inspection.PullRequest.HeadSHA != "h2" {
		t.Fatalf("updated PR=%+v err=%v", inspection.PullRequest, err)
	}
}

func TestForeignKeysRemainEnabledAfterConnectionRecreation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.db.SetMaxIdleConns(0)
	if err := store.db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`INSERT INTO attempts(run_id,number,kind,status,started_at,artifact_dir) VALUES('missing',1,'implementation','started','now','/tmp/missing')`)
	if err == nil {
		t.Fatal("foreign key constraint must survive a recreated connection")
	}
}

func TestRunLeaseUsesOwnerCASAndExpiry(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	if ok, err := store.AcquireLease(context.Background(), "run-1", "owner-1", future); err != nil || !ok {
		t.Fatalf("acquire=%v err=%v", ok, err)
	}
	if ok, err := store.AcquireLease(context.Background(), "run-1", "owner-2", future); err != nil || ok {
		t.Fatalf("competing acquire=%v err=%v", ok, err)
	}
	if ok, err := store.RenewLease(context.Background(), "run-1", "owner-1", future.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("renew=%v err=%v", ok, err)
	}
	if err := store.ReleaseLease(context.Background(), "run-1", "owner-2"); err == nil {
		t.Fatal("wrong owner released lease")
	}
	if err := store.ReleaseLease(context.Background(), "run-1", "owner-1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.AcquireLease(context.Background(), "run-1", "owner-2", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("reacquire=%v err=%v", ok, err)
	}
}

func TestTransitionUsesExpectedStateCompare(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateReceived, domain.StateAdmitting, "admit", "snapshot", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateReceived, domain.StateAdmitting, "duplicate", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateReceived, domain.StateAdmitting, "stale", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateAdmitting, domain.StateProvisioning, "provision", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateAdmitting, domain.StateFailed, "stale", "", ""); err == nil {
		t.Fatal("stale expected state must fail")
	}
}

func TestAttemptArtifactDirectoryCannotBeReused(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginAttempt(context.Background(), "run-1", "implementation", "gpt-5.6-terra", "/tmp/run/attempt-1"); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(context.Background(), "run-1")
	if err != nil || len(inspection.Attempts) != 1 || inspection.Attempts[0].RequestedModel != "gpt-5.6-terra" {
		t.Fatalf("requested model evidence not persisted: inspection=%+v err=%v", inspection, err)
	}
	if _, err := store.BeginAttempt(context.Background(), "run-1", "resume", "gpt-5.6-terra", "/tmp/run/attempt-1"); err == nil {
		t.Fatal("artifact directory reuse must fail")
	}
}

func TestOwnedResourceCannotBeClaimedByAnotherRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for index := 1; index <= 2; index++ {
		id := fmt.Sprintf("run-%d", index)
		input := application.CreateRunInput{Run: application.Run{ID: id, IssueID: fmt.Sprintf("ISSUE-%d", index), IdempotencyKey: fmt.Sprintf("key-%d", index), SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/shared", ArtifactRoot: "/tmp/" + id}}
		if _, _, err := store.CreateRun(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	resource := application.OwnedResource{RunID: "run-1", Kind: "branch", Name: "ifan/shared", CreationEvidence: "{}", Status: "reserved"}
	if err := store.AddOwnedResource(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	resource.RunID = "run-2"
	if err := store.AddOwnedResource(context.Background(), resource); err == nil {
		t.Fatal("second run must not claim an owned branch")
	}
}

func TestBeginRepairAtomicallyRollsCandidateIntoTransition(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "IFAN-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/one", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(ctx, input); err != nil {
		t.Fatal(err)
	}
	states := []domain.State{domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting, domain.StateVerifying, domain.StateFreshReview, domain.StateRepairing}
	current := domain.StateReceived
	for _, next := range states {
		if err := store.Transition(ctx, "run-1", current, next, "test", "", "h1"); err != nil {
			t.Fatal(err)
		}
		current = next
	}
	if err := store.SetCandidateHead(ctx, "run-1", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRepair(ctx, "run-1", "h1", `{"normalized_prompt":"repair","prompt_hash":"hash"}`); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	latest := inspection.Timeline[len(inspection.Timeline)-1]
	if inspection.Run.State != domain.StateExecuting || inspection.Run.CandidateHead != "" || latest.BoundHead != "h1" {
		t.Fatalf("repair rollover=%+v", inspection)
	}
}
