package main

import (
	"fmt"
	"os"
	"strings"
)

type namedPath struct {
	name string
	path string
}

type resolvedNamedPath struct {
	namedPath
	canonical string
	info      os.FileInfo
}

func validateCommandPaths(command string, opts commandOptions) error {
	paths := []namedPath{
		{name: "--policy", path: opts.policy},
		{name: "--report-json", path: opts.reportJSON},
		{name: "--report-md", path: opts.reportMD},
	}
	switch command {
	case "baseline":
		paths = append(paths,
			namedPath{name: "--owners", path: opts.owners},
			namedPath{name: "--baseline", path: opts.baseline},
		)
	case "verify":
		paths = append(paths,
			namedPath{name: "--owners", path: opts.owners},
			namedPath{name: "--baseline", path: opts.baseline},
			namedPath{name: "--workers", path: opts.workers},
		)
	case "workers":
		if opts.unclassified {
			paths = append(paths,
				namedPath{name: "--baseline", path: opts.baseline},
				namedPath{name: "--workers", path: opts.workers},
			)
		}
	}
	return validateDistinctNamedPaths(paths...)
}

func validateDistinctNamedPaths(paths ...namedPath) error {
	resolved := make([]resolvedNamedPath, 0, len(paths))
	for _, candidate := range paths {
		candidate.path = strings.TrimSpace(candidate.path)
		if candidate.path == "" {
			continue
		}
		canonical, err := canonicalOutputPath(candidate.path)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", candidate.name, err)
		}
		var info os.FileInfo
		if stat, err := os.Stat(candidate.path); err == nil {
			info = stat
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", candidate.name, err)
		}
		resolved = append(resolved, resolvedNamedPath{
			namedPath: candidate,
			canonical: canonical,
			info:      info,
		})
	}

	for i := 0; i < len(resolved); i++ {
		for j := i + 1; j < len(resolved); j++ {
			same := resolved[i].canonical == resolved[j].canonical
			if !same && resolved[i].info != nil && resolved[j].info != nil {
				same = os.SameFile(resolved[i].info, resolved[j].info)
			}
			if same {
				return fmt.Errorf("%s and %s must resolve to distinct files", resolved[i].name, resolved[j].name)
			}
		}
	}
	return nil
}
