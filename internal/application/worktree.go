package application

import "context"

type WorktreeSpec struct {
	SourcePath string `json:"source_path"`
	OriginPath string `json:"origin_path"`
	BaseBranch string `json:"base_branch"`
	Branch     string `json:"branch"`
	Path       string `json:"path"`
	Nonce      string `json:"nonce"`
}

type WorktreeRecord struct {
	SourcePath string `json:"source_path"`
	OriginPath string `json:"origin_path"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	BaseSHA    string `json:"base_sha"`
	Nonce      string `json:"nonce"`
}

type WorktreeProvisioner interface {
	Provision(context.Context, WorktreeSpec) (WorktreeRecord, error)
	ValidateOwned(context.Context, WorktreeRecord) error
}
