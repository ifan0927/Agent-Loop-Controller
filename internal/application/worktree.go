package application

import "context"

type WorktreeSpec struct {
	SourcePath string
	OriginPath string
	BaseBranch string
	Branch     string
	Path       string
}

type WorktreeRecord struct {
	SourcePath string `json:"source_path"`
	OriginPath string `json:"origin_path"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	BaseSHA    string `json:"base_sha"`
}

type WorktreeProvisioner interface {
	Provision(context.Context, WorktreeSpec) (WorktreeRecord, error)
	ValidateOwned(context.Context, WorktreeRecord) error
}
