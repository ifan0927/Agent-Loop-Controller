package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localissue"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localregistry"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestDecodeTaskRejectsTrailingJSON(t *testing.T) {
	input := `{"run_id":"one"} {"run_id":"two"}`
	if _, err := decodeTask(strings.NewReader(input)); err == nil {
		t.Fatal("expected trailing JSON to be rejected")
	}
}

func TestLinearRegistryResolverMapsRepositoryLabelToCanonicalRepository(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "source"), filepath.Join(root, "runs"), filepath.Join(root, "worktrees")}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := localregistry.New([]localregistry.Repository{{
		Owner: "IFAN0927", Name: "LoopTest", LinearLabel: "looptest", OriginURL: "git@github.com:ifan0927/LoopTest.git",
		SourcePath: paths[0], RunRoot: paths[1], WorktreeRoot: paths[2], BaseBranch: "main",
		VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"},
		GitHubAppProfileRef: "github-app-profile:fixture", GitHubAppID: 1, GitHubInstallationID: 2, ExpectedRepositoryID: 3,
		OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: []string{"ifan0927"}, TrustedActors: []localregistry.TrustedActorIdentity{{DatabaseID: 1, NodeID: "actor", Login: "ifan0927", Type: "User"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	resolver := linearRegistryResolver{registry: registry}
	repository, ok := resolver.ResolveLinearAdmissionRepository("repo:looptest")
	if !ok || repository.CanonicalRepository != "ifan0927/looptest" {
		t.Fatalf("repository=%+v ok=%t", repository, ok)
	}
	for _, label := range []string{"ifan0927/looptest", "repo:", "repo:ifan0927/looptest", "repo:other"} {
		if _, ok := resolver.ResolveLinearAdmissionRepository(label); ok {
			t.Fatalf("unexpected resolution for %q", label)
		}
	}
}

func TestPersistedBindingRejectsCrossRepositorySwap(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repositories := make([]localregistry.Repository, 0, 2)
	for index, name := range []string{"one", "two"} {
		base := filepath.Join(root, name)
		paths := []string{filepath.Join(base, "origin"), filepath.Join(base, "source"), filepath.Join(base, "runs"), filepath.Join(base, "worktrees")}
		for _, path := range paths {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		repositories = append(repositories, localregistry.Repository{Owner: "owner", Name: name, OriginPath: paths[0], SourcePath: paths[1], RunRoot: paths[2], WorktreeRoot: paths[3], BaseBranch: "main", VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"}, GitHubAppProfileRef: "github-app-profile:fixture", GitHubAppID: 10, GitHubInstallationID: int64(index + 1), ExpectedRepositoryID: int64(index + 101), OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: []string{"ifan0927"}, TrustedActors: []localregistry.TrustedActorIdentity{{DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", Login: "ifan0927", Type: "User"}}}})
	}
	registryRaw, _ := json.Marshal(localregistry.File{Version: 1, Repositories: repositories})
	registryPath := filepath.Join(root, "registry.json")
	if err := os.WriteFile(registryPath, registryRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := localregistry.Load(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	issue := localissue.Issue{IssueID: "ISSUE-1", Title: "task", Description: "test", Team: "IFAN", Labels: []string{"agent:codex", "owner/one"}, Status: "Todo", CurrentCycle: true, CycleID: "cycle", RepositoryLabel: "owner/one", BaseBranch: "main", BranchName: "ifan/test", Goal: "test", AcceptanceCriteria: []string{"test"}, VerifierIDs: []string{"fixture-go-test"}, SourceRevision: "v1", CreatedAt: now, UpdatedAt: now}
	rawIssue, _ := json.Marshal(issue)
	snapshot, err := localissue.Admit(issue, rawIssue, registry)
	if err != nil {
		t.Fatal(err)
	}
	bindingTwo, _ := registry.Resolve("owner/two")
	appBinding := localRepository(bindingTwo)
	bindingRaw, _ := json.Marshal(appBinding)
	taskTwo := snapshot.Task
	taskTwo.Repository = "owner/two"
	taskRaw, _ := json.Marshal(taskTwo)
	taskHash := sha256.Sum256(taskRaw)
	run := application.Run{ID: snapshot.Task.RunID, IssueID: issue.IssueID, IdempotencyKey: snapshot.IdempotencyKey, SourceRevision: "v1", RawIssueJSON: string(rawIssue), RawIssueHash: snapshot.RawHash,
		Repository: "owner/two", RepositoryConfigJSON: string(bindingRaw), ProfileID: bindingTwo.ProfileID, ProfileSnapshotVersion: bindingTwo.ProfileSnapshotVersion, ProfileDigest: bindingTwo.ProfileDigest, ProfileSnapshotJSON: bindingTwo.ProfileSnapshotJSON, RegistryVersion: bindingTwo.RegistryVersion, RegistryDigest: bindingTwo.RegistryDigest, RepositoryBindingDigest: bindingTwo.RepositoryBindingDigest,
		BaseBranch: "main", WorkingBranch: "ifan/test", NormalizedTaskJSON: string(taskRaw), TaskHash: fmt.Sprintf("%x", taskHash), WorktreePath: filepath.Join(bindingTwo.WorktreeRoot, snapshot.Task.RunID), ArtifactRoot: filepath.Join(bindingTwo.RunRoot, snapshot.Task.RunID)}
	if err := validatePersistedRegistryBinding(run, registry); err == nil || !strings.Contains(err.Error(), "canonical issue admission") {
		t.Fatalf("cross-repository persisted binding swap error=%v", err)
	}
}

func TestProductionPersistedBindingAcceptsLinearSnapshot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "origin"), filepath.Join(root, "source"), filepath.Join(root, "runs"), filepath.Join(root, "worktrees")}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	registryRaw, _ := json.Marshal(localregistry.File{Version: 1, Repositories: []localregistry.Repository{{Owner: "owner", Name: "repo", OriginPath: paths[0], SourcePath: paths[1], RunRoot: paths[2], WorktreeRoot: paths[3], BaseBranch: "main", VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"}, GitHubAppProfileRef: "github-app-profile:fixture", GitHubAppID: 1, GitHubInstallationID: 2, ExpectedRepositoryID: 3, OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: []string{"ifan0927"}, TrustedActors: []localregistry.TrustedActorIdentity{{DatabaseID: 1, NodeID: "actor", Login: "ifan0927", Type: "User"}}}}}})
	registryPath := filepath.Join(root, "registry.json")
	if err := os.WriteFile(registryPath, registryRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := localregistry.Load(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := registry.Resolve("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	task := domain.CodingTask{RunID: "run-linear", IssueID: "IFAN-42", IssueURL: "https://linear.app/ifan/issue/IFAN-42/test", Title: "Task", Description: "## Goal\nTask\n## Acceptance Criteria\n- Pass", Repository: "owner/repo", BaseBranch: "main", WorkingBranch: "ifan/ifan-42", Goal: "Task", AcceptanceCriteria: []string{"Pass"}, VerifierIDs: []string{"fixture-go-test"}, Policy: domain.TaskPolicy{HumanApprovalRequired: true, MergeMethod: "squash"}, SourceRevision: "revision", CreatedAt: time.Now().UTC()}
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}
	normalized, _ := json.Marshal(task)
	raw := []byte(`{"provider":"linear","identifier":"IFAN-42"}`)
	rawDigest, taskDigest := sha256.Sum256(raw), sha256.Sum256(normalized)
	repository := localRepository(binding)
	repositoryRaw, _ := json.Marshal(repository)
	run := application.Run{ID: task.RunID, IssueID: task.IssueID, IdempotencyKey: "key", SourceRevision: task.SourceRevision, RawIssueJSON: string(raw), RawIssueHash: hex.EncodeToString(rawDigest[:]), NormalizedTaskJSON: string(normalized), TaskHash: hex.EncodeToString(taskDigest[:]), Repository: task.Repository, RepositoryConfigJSON: string(repositoryRaw), ProfileID: repository.ProfileID, ProfileSnapshotVersion: repository.ProfileSnapshotVersion, ProfileDigest: repository.ProfileDigest, ProfileSnapshotJSON: repository.ProfileSnapshotJSON, RegistryVersion: repository.RegistryVersion, RegistryDigest: repository.RegistryDigest, RepositoryBindingDigest: repository.RepositoryBindingDigest, BaseBranch: task.BaseBranch, WorkingBranch: task.WorkingBranch, WorktreePath: filepath.Join(repository.WorktreeRoot, task.RunID), ArtifactRoot: filepath.Join(repository.RunRoot, task.RunID)}
	if err := validateProductionPersistedBinding(run, registry); err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeInspectionRemovesNestedSensitiveEvidence(t *testing.T) {
	secret := "/secret/evidence"
	inspection := application.RunInspection{
		Run:           application.Run{WorktreePath: secret, ArtifactRoot: secret, LastError: secret},
		Timeline:      []application.Transition{{EvidenceReference: secret}},
		Attempts:      []application.Attempt{{SessionID: secret, StdoutPath: secret, StderrPath: secret, OutcomePath: secret, ArtifactDir: secret}},
		Verifications: []application.VerificationRecord{{StdoutPath: secret, StderrPath: secret, EvidencePath: secret}},
		Reviews:       []application.ReviewRecord{{SessionID: secret, OutcomePath: secret}},
		Resources:     []application.OwnedResource{{Name: secret, CreationEvidence: secret}},
		SideEffects:   []application.SideEffectRecord{{IntentJSON: secret, ResultJSON: secret, StdoutPath: secret, StderrPath: secret}},
		Polls:         []application.PollObservation{{SnapshotJSON: secret}},
		Findings:      []application.FindingRecord{{Body: secret, File: secret}},
		Cleanup:       []application.CleanupRecord{{Name: secret, LastError: secret}},
	}
	sanitizeInspection(&inspection)
	raw, _ := json.Marshal(inspection)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("sanitized inspection leaked nested evidence: %s", raw)
	}
}

func TestLocalCommandsAcceptDocumentedLeadingRunID(t *testing.T) {
	runID, args := splitLeadingRunID([]string{"run-123", "--db", "/tmp/controller.db"})
	if runID != "run-123" || len(args) != 2 || args[0] != "--db" {
		t.Fatalf("runID=%q args=%v", runID, args)
	}
}

func TestNormalizeControllerDatabasePathAcceptsRelativeCLIPath(t *testing.T) {
	path, err := normalizeControllerDatabasePath("./controller.db")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != "controller.db" {
		t.Fatalf("normalized database path=%q", path)
	}
}

func TestLocalContinueRequiresCallerCASExpectations(t *testing.T) {
	err := localContinue([]string{"run-123", "--db", "/unused/controller.db", "--registry", "/unused/registry.json", "--requester", "ifan0927", "--repository", "owner/repo"})
	if err == nil || !strings.Contains(err.Error(), "--expected-state") || !strings.Contains(err.Error(), "--idempotency-key") {
		t.Fatalf("missing explicit CAS error=%v", err)
	}
}

func TestLinearStartRequiresExplicitIssueAndRequesterEvidence(t *testing.T) {
	if err := linearStart(nil); err == nil || !strings.Contains(err.Error(), "IFAN issue identifier") {
		t.Fatalf("missing input error=%v", err)
	}
}

func TestLinearStartAcceptsIssueBeforeFlags(t *testing.T) {
	identifier, remaining := splitLinearStartIdentifier([]string{"IFAN-42", "--config", "linear.json"})
	if identifier != "IFAN-42" || !slices.Equal(remaining, []string{"--config", "linear.json"}) {
		t.Fatalf("identifier=%q remaining=%q", identifier, remaining)
	}
}

func TestControllerRunRequiresExplicitIssueAndRequesterIdentity(t *testing.T) {
	err := controller([]string{"run", "IFAN-42"})
	if err == nil || !strings.Contains(err.Error(), "complete requester identity") {
		t.Fatalf("missing controller run requester identity error=%v", err)
	}
}

func TestControllerDriveRequiresRunAndRequesterIdentity(t *testing.T) {
	err := controller([]string{"drive"})
	if err == nil || !strings.Contains(err.Error(), "run ID and complete requester identity") {
		t.Fatalf("missing controller drive argument error=%v", err)
	}
}

func TestControllerAbandonRequiresRunAndRequesterIdentity(t *testing.T) {
	err := controller([]string{"abandon"})
	if err == nil || !strings.Contains(err.Error(), "run ID and complete requester identity") {
		t.Fatalf("missing controller abandon argument error=%v", err)
	}
}

func TestControllerAbandonRejectsHumanDecisionFlag(t *testing.T) {
	err := controller([]string{"abandon", "run-abandon", "--decision", "/tmp/decision.json"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -decision") {
		t.Fatalf("abandon accepted human decision input error=%v", err)
	}
}

func TestControllerAbandonRejectsCallerSuppliedRunAuthority(t *testing.T) {
	err := controller([]string{"abandon", "run-abandon", "--expected-state", "manual_intervention"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -expected-state") {
		t.Fatalf("abandon accepted caller-supplied run authority error=%v", err)
	}
}

func TestControllerDriveRejectsUnsafeAutomaticDriverOptionsBeforeConfigLoad(t *testing.T) {
	identity := []string{"--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User"}
	err := controller(append([]string{"drive", "run-42", "--poll-interval", "0s"}, identity...))
	if err == nil || !strings.Contains(err.Error(), "--poll-interval must be positive") {
		t.Fatalf("poll interval error=%v", err)
	}
	err = controller(append([]string{"drive", "run-42", "--max-immediate-actions", "0"}, identity...))
	if err == nil || !strings.Contains(err.Error(), "--max-immediate-actions must be positive") {
		t.Fatalf("immediate action error=%v", err)
	}
	err = controller(append([]string{"drive", "run-42", "--max-runtime", "169h"}, identity...))
	if err == nil || !strings.Contains(err.Error(), "--max-runtime") {
		t.Fatalf("runtime error=%v", err)
	}
}

func TestLocalContinueAuthorizesBeforeRegistryRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := json.Marshal(application.LocalRepository{AllowedOperatorLogins: []string{"ifan0927"}})
	_, _, err = store.CreateRun(context.Background(), application.CreateRunInput{Run: application.Run{ID: "run-auth-first", IdempotencyKey: "key", Repository: "owner/repo", RepositoryConfigJSON: string(authority)}, ConfigurationAuthority: testConfigurationAuthority(t, store, path)})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = localContinue([]string{"run-auth-first", "--db", path, "--registry", filepath.Join(t.TempDir(), "missing.json"), "--requester", "intruder", "--requester-database-id", "44", "--requester-node-id", "intruder-node", "--requester-type", "User", "--repository", "owner/repo", "--expected-state", "received", "--idempotency-key", "key"})
	if err == nil || !strings.Contains(err.Error(), "not_found: resource is not available") {
		t.Fatalf("unauthorized continue exposed registry error=%v", err)
	}
}

func TestLocalContinueRejectsCallerRepositoryBeforeRegistryRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := json.Marshal(application.LocalRepository{AllowedOperatorLogins: []string{"ifan0927"}})
	_, _, err = store.CreateRun(context.Background(), application.CreateRunInput{Run: application.Run{ID: "run-repository", IdempotencyKey: "key", Repository: "owner/repo", RepositoryConfigJSON: string(authority)}, ConfigurationAuthority: testConfigurationAuthority(t, store, path)})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = localContinue([]string{"run-repository", "--db", path, "--registry", filepath.Join(t.TempDir(), "missing.json"), "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User", "--repository", "owner/other", "--expected-state", "received", "--idempotency-key", "key"})
	if err == nil || !strings.Contains(err.Error(), "not_found: resource is not available") {
		t.Fatalf("repository mismatch exposed registry error=%v", err)
	}
}

func TestDecodeDecisionRejectsTrailingJSON(t *testing.T) {
	if _, err := decodeDecision(strings.NewReader(`{"choice_id":"a","instructions":"go"} {}`)); err == nil {
		t.Fatal("expected trailing decision JSON rejection")
	}
}

func TestExternalJSONCannotOverrideModelPolicy(t *testing.T) {
	if _, err := decodeTask(strings.NewReader(`{"model":"gpt-5.6"}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("task model override error=%v", err)
	}
	if _, err := decodeDecision(strings.NewReader(`{"choice_id":"a","instructions":"go","model":"gpt-5.6-sol"}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("decision model override error=%v", err)
	}
}

func TestLocalStatusOutputsDurableInspection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := json.Marshal(application.LocalRepository{AllowedOperatorLogins: []string{"ifan0927"}})
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw-hash", NormalizedTaskJSON: "{}", TaskHash: "task-hash", Repository: "repo:test-project", RepositoryConfigJSON: string(authority), BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run", ImplementationModel: "gpt-5.6-terra", ReviewModel: "gpt-5.6-sol"}}
	input.ConfigurationAuthority = testConfigurationAuthority(t, store, path)
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	store.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	callErr := localInspect("status", []string{"run-1", "--db", path, "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User"})
	write.Close()
	os.Stdout = original
	if callErr != nil {
		t.Fatal(callErr)
	}
	output, err := io.ReadAll(read)
	read.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"current_state": "received"`, `"idempotency_key": "key"`, `"implementation_model": "gpt-5.6-terra"`, `"review_model": "gpt-5.6-sol"`, `"state_timeline"`, `"task_snapshot_hash": "task-hash"`, `"attempts"`, `"verifications"`, `"reviews"`, `"owned_resources"`} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("status output missing %s: %s", want, output)
		}
	}
}

func TestControllerStatusProjectsIdempotencyKeyOnlyToAuthorizedOperator(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configPath, dbPath := writeControllerStatusConfig(t, root)
	if _, err := loadManagedConfiguration(configPath); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := json.Marshal(application.LocalRepository{AllowedOperatorLogins: []string{"ifan0927"}, TrustedOperatorActors: []application.TrustedActorIdentity{{DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", Login: "ifan0927", Type: "User"}}})
	run := application.Run{ID: "run-status", IssueID: "IFAN-18", IdempotencyKey: "resume-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: string(authority), BaseBranch: "main", WorkingBranch: "ifan/ifan-18"}
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run, ConfigurationAuthority: testExistingNewAdmissionGate(t, store).Decision.Authority}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	callErr := controller([]string{"status", "run-status", "--config", configPath, "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User"})
	write.Close()
	os.Stdout = original
	if callErr != nil {
		t.Fatal(callErr)
	}
	output, err := io.ReadAll(read)
	read.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `"idempotency_key": "resume-key"`) || strings.Contains(string(output), "private-key-material") {
		t.Fatalf("controller status output=%s", output)
	}

	if err := controller([]string{"inspect", "run-status", "--config", configPath, "--requester", "intruder", "--requester-database-id", "44", "--requester-node-id", "intruder-node", "--requester-type", "User"}); err == nil || !strings.Contains(err.Error(), "not_found: resource is not available") {
		t.Fatalf("unauthorized controller inspect error=%v", err)
	}
}

func writeControllerStatusConfig(t *testing.T, root string) (configPath, dbPath string) {
	t.Helper()
	paths := []string{filepath.Join(root, "origin"), filepath.Join(root, "source"), filepath.Join(root, "runs"), filepath.Join(root, "worktrees")}
	for _, path := range paths {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dbPath = filepath.Join(root, "controller.db")
	privateKeyPath := filepath.Join(root, "app.pem")
	if err := os.WriteFile(privateKeyPath, []byte("private-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := localregistry.File{Version: 1, Repositories: []localregistry.Repository{{
		Owner: "owner", Name: "repo", OriginPath: paths[0], SourcePath: paths[1], RunRoot: paths[2], WorktreeRoot: paths[3],
		BaseBranch: "main", VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"},
		GitHubAppProfileRef: "github-app-profile:fixture", GitHubAppID: 1, GitHubInstallationID: 2, ExpectedRepositoryID: 3,
		OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: []string{"ifan0927"}, TrustedActors: []localregistry.TrustedActorIdentity{{DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", Login: "ifan0927", Type: "User"}}},
	}}}
	registryRaw, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(root, "registry.json")
	if err := os.WriteFile(registryPath, registryRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	githubConfig := map[string]any{"api_base_url": "https://api.github.com", "graphql_url": "https://api.github.com/graphql", "app_id": 1, "installation_id": 2, "repository_owner": "owner", "repository_name": "repo", "repository_id": 3, "private_key_file": privateKeyPath, "http_timeout": "2s", "token_refresh_skew": "5m", "api_version": "2022-11-28"}
	config := map[string]any{"version": 1, "controller": map[string]any{"database_path": dbPath, "codex_binary": "codex", "run_timeout": "30m"}, "linear": map[string]any{"api_url": "https://api.linear.app/graphql", "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN", "authorization_scheme": "bearer", "team_key": "IFAN", "http_timeout": "2s", "max_response_bytes": 4096, "label_page_size": 10, "max_label_pages": 1}, "repository_registry_file": registryPath, "github_app_profiles": []map[string]any{{"id": "github-app-profile:fixture", "config": githubConfig}}}
	configRaw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, configRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, dbPath
}

func TestManagedConfigurationUsesTrustedLocatorAfterLiveDrift(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeControllerStatusConfig(t, root)
	baseline, err := loadManagedConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Controller.DatabasePath != databasePath {
		t.Fatalf("baseline database=%q", baseline.Controller.DatabasePath)
	}
	if err := os.WriteFile(configPath, []byte(`{"version":99,"controller":{"database_path":"/tmp/attacker.db"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	retained, err := loadManagedConfiguration(configPath)
	if err != nil || retained.Digest != baseline.Digest || retained.Controller.DatabasePath != databasePath {
		t.Fatalf("retained=%+v err=%v", retained, err)
	}
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, found, err := store.ConfigurationAuthority(context.Background())
	if err != nil || !found || authority.Desired.Digest != baseline.Digest || authority.CanonicalConfigPath != configPath {
		t.Fatalf("authority=%+v found=%t err=%v", authority, found, err)
	}
}

func TestManualSupervisorHeartbeatMakesAdmissionReadyWhileHoldingWorkerLock(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeControllerStatusConfig(t, root)
	loaded, err := loadManagedConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireWorkerProcessLock(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	_, heartbeat, err := startManualControllerHeartbeat(context.Background(), loaded.Path, loaded.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer heartbeat.Stop()
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	convergence, err := configuredConvergenceService(store, loaded, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := convergence.CheckNewAdmission(context.Background())
	if err != nil || !decision.Allowed || decision.Reason != application.ConfigurationReasonReady || decision.Authority.Digest != loaded.Digest {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestManagedConfigurationRejectsAlternatePathForOwnedStore(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, _ := writeControllerStatusConfig(t, root)
	if _, err := loadManagedConfiguration(configPath); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(root, "alternate.json")
	if err := os.WriteFile(alternate, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManagedConfiguration(alternate); err == nil || !strings.Contains(err.Error(), "locator conflicts") {
		t.Fatalf("alternate path error=%v", err)
	}
}

func TestManagedConfigurationFinishesBaselineAfterLocatorPublicationCrash(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeControllerStatusConfig(t, root)
	loadedBeforeCrash, err := bootstrap.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	files, err := configurationadapter.NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := files.ValidateBaseline(payload)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.BindDatabaseIdentity(databasePath, store.DatabaseIdentity()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := files.RetainRaw(candidate.Digest, payload); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := files.PublishBaselineBinding(candidate); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.PrepareConfigurationBaseline(context.Background(), application.ConfigurationBaselineInput{Candidate: candidate, CanonicalConfigPath: configPath, ObservedAt: time.Now().UTC()}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := files.PublishLocator(databasePath); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadManagedConfiguration(configPath)
	if err != nil || loaded.Controller.DatabasePath != databasePath || loaded.Digest != loadedBeforeCrash.Digest {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	store, err = sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if authority, found, err := store.ConfigurationAuthority(context.Background()); err != nil || !found || authority.Desired.Digest != loaded.Digest {
		t.Fatalf("authority=%+v found=%t err=%v", authority, found, err)
	}
}

func TestManagedConfigurationResumesBaselineBindingBeforeDatabaseAnchor(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeControllerStatusConfig(t, root)
	payload, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	files, err := configurationadapter.NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := files.ValidateBaseline(payload)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.BindDatabaseIdentity(databasePath, store.DatabaseIdentity()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := files.RetainRaw(candidate.Digest, payload); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := files.PublishBaselineBinding(candidate); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadManagedConfiguration(configPath)
	if err != nil || loaded.Digest != candidate.Digest || loaded.Controller.DatabasePath != databasePath {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	store, err = sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if authority, found, err := store.ConfigurationAuthority(context.Background()); err != nil || !found || authority.Desired.Digest != candidate.Digest {
		t.Fatalf("authority=%+v found=%t err=%v", authority, found, err)
	}
}

func TestManagedConfigurationRejectsLiveRelocationAfterPreparedBaseline(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeControllerStatusConfig(t, root)
	payload, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	files, err := configurationadapter.NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := files.ValidateBaseline(payload)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.BindDatabaseIdentity(databasePath, store.DatabaseIdentity()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := files.RetainRaw(candidate.Digest, payload); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := files.PublishBaselineBinding(candidate); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.PrepareConfigurationBaseline(context.Background(), application.ConfigurationBaselineInput{Candidate: candidate, CanonicalConfigPath: configPath, ObservedAt: time.Now().UTC()}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var changed map[string]any
	if err := json.Unmarshal(payload, &changed); err != nil {
		t.Fatal(err)
	}
	attackerDatabase := filepath.Join(root, "attacker.db")
	changed["controller"].(map[string]any)["database_path"] = attackerDatabase
	changedPayload, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, changedPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManagedConfiguration(configPath); err == nil || !strings.Contains(err.Error(), "baseline binding conflicts") {
		t.Fatalf("baseline relocation error=%v", err)
	}
	if _, err := os.Lstat(attackerDatabase); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("alternate database was created: %v", err)
	}
	boundConfig, boundDatabase, bound, err := sqlitestore.InspectConfigurationBindingReadOnly(context.Background(), databasePath)
	if err != nil || !bound || boundConfig != configPath || boundDatabase != databasePath {
		t.Fatalf("bound config=%q database=%q bound=%t err=%v", boundConfig, boundDatabase, bound, err)
	}
}

func TestManagedConfigurationRejectsUnboundLocatorTarget(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, _ := writeControllerStatusConfig(t, root)
	attackerDatabase := filepath.Join(root, "attacker.db")
	files, err := configurationadapter.NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.PublishLocator(attackerDatabase); err == nil {
		t.Fatal("unbound locator target was published")
	}
	if _, err := os.Lstat(attackerDatabase); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locator target was created: %v", err)
	}
}

func TestLocalStatusRejectsUnauthorizedRequester(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	authority, _ := json.Marshal(application.LocalRepository{AllowedOperatorLogins: []string{"ifan0927"}})
	_, _, err = store.CreateRun(context.Background(), application.CreateRunInput{Run: application.Run{ID: "run-auth", IdempotencyKey: "key", Repository: "owner/repo", RepositoryConfigJSON: string(authority)}, ConfigurationAuthority: testConfigurationAuthority(t, store, path)})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = localInspect("status", []string{"run-auth", "--db", path, "--requester", "intruder", "--requester-database-id", "44", "--requester-node-id", "intruder-node", "--requester-type", "User"})
	if err == nil || !strings.Contains(err.Error(), "not_found: resource is not available") {
		t.Fatalf("unauthorized status error=%v", err)
	}
}

func TestLocalInspectSanitizesRepositoryBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	binding := application.LocalRepository{RegistryVersion: 1, RegistryDigest: "registry-digest", RepositoryBindingDigest: "binding-digest",
		ProfileID: "repository-profile:owner/repo", ProfileSnapshotVersion: 1, ProfileDigest: "profile-digest",
		CanonicalRepository: "owner/repo", OriginPath: "/secret/origin", SourcePath: "/secret/source", RunRoot: "/secret/runs", WorktreeRoot: "/secret/worktrees",
		BaseBranch: "main", VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"}, GitHubAppProfileRef: "github-app-profile:fixture", GitHubAppID: 11,
		GitHubInstallationID: 22, ExpectedRepositoryID: 33, AllowedOperatorLogins: []string{"ifan0927"}, TrustedOperatorActors: []application.TrustedActorIdentity{{DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", Login: "ifan0927", Type: "User"}}}
	raw, _ := json.Marshal(binding)
	input := application.CreateRunInput{Run: application.Run{ID: "run-binding", IssueID: "ISSUE-2", IdempotencyKey: "binding-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: string(raw), ProfileID: binding.ProfileID, ProfileSnapshotVersion: binding.ProfileSnapshotVersion, ProfileDigest: binding.ProfileDigest, ProfileSnapshotJSON: `{}`, RegistryVersion: 1, RegistryDigest: "registry-digest", RepositoryBindingDigest: "binding-digest", BaseBranch: "main", WorkingBranch: "ifan/test", WorktreePath: "/secret/run-worktree", ArtifactRoot: "/secret/artifact"}}
	input.ConfigurationAuthority = testConfigurationAuthority(t, store, path)
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	store.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	callErr := localInspect("inspect", []string{"run-binding", "--db", path, "--requester", "ifan0927", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User"})
	write.Close()
	os.Stdout = original
	if callErr != nil {
		t.Fatal(callErr)
	}
	output, _ := io.ReadAll(read)
	text := string(output)
	for _, secretPath := range []string{"/secret/origin", "/secret/source", "/secret/runs", "/secret/run-worktree", "/secret/artifact"} {
		if strings.Contains(text, secretPath) {
			t.Fatalf("inspect leaked %s: %s", secretPath, text)
		}
	}
	if !strings.Contains(text, `"expected_repository_id": 33`) {
		t.Fatalf("inspection omitted sanitized binding: %s", text)
	}
	if !strings.Contains(text, `"profile_id": "repository-profile:owner/repo"`) || !strings.Contains(text, `"profile_digest": "profile-digest"`) {
		t.Fatalf("inspection omitted profile evidence: %s", text)
	}
}

func TestPreviousObservedPushRequiresMatchingOwnedEvidence(t *testing.T) {
	records := []application.SideEffectRecord{{Kind: "push", Status: "observed", ResultJSON: `{"pushed_sha":"old"}`}, {Kind: "push", Status: "failed", ResultJSON: `{"pushed_sha":"new"}`}}
	if !previousObservedPush(records, "old") {
		t.Fatal("matching observed push not found")
	}
	if previousObservedPush(records, "new") {
		t.Fatal("failed push treated as evidence")
	}
	if previousObservedPush(records, "other") {
		t.Fatal("unknown SHA treated as evidence")
	}
}
