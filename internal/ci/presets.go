package ci

import (
	"fmt"
	"sort"
	"strings"
)

// preset builds one Job from a workflow's `args` map. It is the "sugar" that
// lets a repo declare `preset: npm-publish` instead of a full Job.
type preset struct {
	// build returns the configured job (Its ID is assigned by the expander).
	build func(args map[string]string) (Job, error)
}

// publishProtocols are the protocols that have a built-in publish template
// (internal/publish). Each yields a `<proto>-publish` preset.
var publishProtocols = []string{
	"npm", "pypi", "cargo", "rubygems", "helm", "nuget", "conan", "pub",
	"maven", "go", "hex", "composer", "swift", "generic",
}

// presets is the registry of named presets. Expand assigns the job id.
var presets = func() map[string]preset {
	m := map[string]preset{}
	for _, p := range publishProtocols {
		proto := p
		m[proto+"-publish"] = preset{build: func(args map[string]string) (Job, error) {
			job := Job{
				RunsOn: []string{"os=linux", "is_container=true"},
				Steps:  []Step{{Name: "publish", Run: "true"}},
				Produce: Produce{
					Action:   ActionPublishProtocol,
					Protocol: proto,
					Name:     args["name"],
					Version:  args["version"],
					File:     args["file"],
				},
			}
			// Protocols whose templates require explicit args must have them;
			// manifest-driven protocols (npm/pypi/...) infer them in-container.
			for _, req := range []string{"name", "version", "file"} {
				if requiredArg(proto, req) && args[req] == "" {
					return Job{}, fmt.Errorf("preset %s-publish requires args.%s", proto, req)
				}
			}
			return job, nil
		}}
	}
	m["container-build"] = preset{build: func(args map[string]string) (Job, error) {
		if args["tag"] == "" {
			return Job{}, fmt.Errorf("preset container-build requires args.tag")
		}
		return Job{
			RunsOn: []string{"os=linux", "is_container=true"},
			Steps:  []Step{{Name: "build", Run: "true"}},
			Produce: Produce{
				Action:        ActionOCIBuild,
				Tag:           args["tag"],
				Dockerfile:    args["dockerfile"],
				Context:       args["context"],
				Containerfile: args["containerfile"],
			},
		}, nil
	}}
	return m
}()

// requiredArgs maps protocols whose publish template needs explicit NAME/
// VERSION/FILE (mirrors internal/publish's spec.required).
var requiredArgs = map[string][]string{
	"maven":    {"name", "version"},
	"go":       {"name", "version"},
	"hex":      {"name", "version"},
	"composer": {"name", "version"},
	"swift":    {"name", "version"},
	"generic":  {"name", "version", "file"},
}

func requiredArg(proto, arg string) bool {
	for _, a := range requiredArgs[proto] {
		if a == arg {
			return true
		}
	}
	return false
}

// Presets lists the supported preset names, sorted.
func Presets() []string {
	out := make([]string, 0, len(presets))
	for name := range presets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ExpandPreset expands a named preset + args into a Job with the given id.
func ExpandPreset(name, id string, args map[string]string) (Job, error) {
	p, ok := presets[name]
	if !ok {
		return Job{}, fmt.Errorf("unknown preset %q (supported: %s)", name, strings.Join(Presets(), ", "))
	}
	if args == nil {
		args = map[string]string{}
	}
	job, err := p.build(args)
	if err != nil {
		return Job{}, err
	}
	if id == "" {
		id = strings.TrimSuffix(name, "-publish")
	}
	job.ID = id
	return job, nil
}
