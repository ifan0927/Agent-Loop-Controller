package main

import (
	"context"
	"errors"
	"flag"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
)

type credentialChecker interface {
	Check(context.Context) error
}

type runtimeDoctorOutput struct {
	CredentialReady bool   `json:"linear_credential_ready"`
	Warning         string `json:"warning,omitempty"`
}

// runtimeDoctor checks only safe credential-source topology. It never reads
// token bytes, performs network I/O, or returns a path, reference, or cause.
func runtimeDoctor(args []string) error {
	flags := flag.NewFlagSet("config doctor", flag.ContinueOnError)
	pathFlag := configPathFlag(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config doctor does not accept positional arguments")
	}
	path, err := resolveConfigPath(*pathFlag)
	if err != nil {
		return err
	}
	loaded, err := bootstrap.Load(path)
	if err != nil {
		return err
	}
	source, err := linearCredentialSource(loaded)
	if err != nil {
		return printJSON(runtimeDoctorOutput{Warning: "Linear credential source is unavailable"})
	}
	checker, ok := source.(credentialChecker)
	if !ok || checker.Check(context.Background()) != nil {
		return printJSON(runtimeDoctorOutput{Warning: "Linear credential source is unavailable"})
	}
	return printJSON(runtimeDoctorOutput{CredentialReady: true})
}

// linearCredentialSource is the only composition point for Linear runtime
// credentials. A file reference can never silently fall back to the process
// environment.
func linearCredentialSource(loaded bootstrap.Bootstrap) (linearadapter.CredentialSource, error) {
	return linearCredentialSourceForRef(loaded, loaded.Linear.CredentialSourceRef)
}

func linearCredentialSourceForRef(loaded bootstrap.Bootstrap, ref string) (linearadapter.CredentialSource, error) {
	switch ref {
	case linearadapter.EnvironmentCredentialSourceRef:
		return linearadapter.EnvironmentCredentialSource{Variable: "IFAN_LOOP_LINEAR_TOKEN"}, nil
	case linearadapter.FileCredentialSourceRef:
		return linearadapter.FileCredentialSource{Root: loaded.CredentialDirectory}, nil
	default:
		return nil, errors.New("Linear credential source is unavailable")
	}
}
