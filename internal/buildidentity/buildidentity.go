package buildidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime/debug"
	"strconv"
	"strings"
)

const SchemaVersion = 1

// Info is the allowlisted build evidence exposed by agentctl version --json.
type Info struct {
	ProductVersion                   string `json:"product_version"`
	BuildIdentity                    string `json:"build_identity"`
	VCSRevision                      string `json:"vcs_revision"`
	VCSTime                          string `json:"vcs_time"`
	VCSModified                      bool   `json:"vcs_modified"`
	SupportedControllerSchemaVersion int    `json:"supported_controller_database_schema_version"`
}

func Current(productVersion string, supportedSchemaVersion int) Info {
	info := Info{ProductVersion: productVersion, SupportedControllerSchemaVersion: supportedSchemaVersion}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				info.VCSRevision = strings.ToLower(setting.Value)
			case "vcs.time":
				info.VCSTime = setting.Value
			case "vcs.modified":
				info.VCSModified = setting.Value == "true"
			}
		}
	}
	payload := strings.Join([]string{
		"agentctl-build-identity-v" + strconv.Itoa(SchemaVersion),
		info.ProductVersion,
		info.VCSRevision,
		info.VCSTime,
		strconv.FormatBool(info.VCSModified),
		strconv.Itoa(info.SupportedControllerSchemaVersion),
	}, "\n")
	digest := sha256.Sum256([]byte(payload))
	info.BuildIdentity = "sha256:" + hex.EncodeToString(digest[:])
	return info
}
