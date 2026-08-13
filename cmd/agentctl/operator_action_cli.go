package main

import (
	"errors"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

// composeOperatorActionService is the shared authenticated CLI composition
// boundary for the later typed retry and abandon commands. It deliberately
// accepts no action prose, transport payload, or executable input.
func composeOperatorActionService(store *sqlitestore.Store) (*application.OperatorActionService, error) {
	if store == nil {
		return nil, errors.New("operator action store is required")
	}
	return application.NewOperatorActionService(store)
}
