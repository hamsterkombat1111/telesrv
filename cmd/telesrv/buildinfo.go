package main

import "runtime/debug"

var (
	gitCommit    = ""
	gitBranch    = ""
	gitTreeState = ""
	buildTime    = ""
)

type buildMetadata struct {
	Commit    string
	Branch    string
	TreeState string
	BuildTime string
	GoVersion string
}

func currentBuildMetadata() buildMetadata {
	meta := buildMetadata{
		Commit:    valueOrUnknown(gitCommit),
		Branch:    valueOrUnknown(gitBranch),
		TreeState: valueOrUnknown(gitTreeState),
		BuildTime: valueOrUnknown(buildTime),
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		meta.GoVersion = info.GoVersion
		settings := map[string]string{}
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
		if meta.Commit == "unknown" {
			meta.Commit = valueOrUnknown(settings["vcs.revision"])
		}
		if meta.TreeState == "unknown" {
			switch settings["vcs.modified"] {
			case "true":
				meta.TreeState = "dirty"
			case "false":
				meta.TreeState = "clean"
			}
		}
	}
	meta.GoVersion = valueOrUnknown(meta.GoVersion)
	return meta
}

func valueOrUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}
