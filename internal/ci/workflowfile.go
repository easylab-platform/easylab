package ci

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// WorkflowFile is the parsed shape of a repo's `.easylab/workflows.yaml`: a
// declarative source of Workflows that is a YAML mirror of the
// easylab.v1.Workflow proto plus a `preset` convenience. When a workflow uses
// `jobs`, it is strictly 1:1 with the RPC message; when it uses `preset`, the
// job is expanded (see presets.go). org/repo/branch are filled by the caller
// (the file's location), mirroring how the RPC carries them explicitly.

type workflowFile struct {
	Version   int           `yaml:"version"`
	Workflows []workflowDoc `yaml:"workflows"`
}

type workflowDoc struct {
	Name   string            `yaml:"name"`
	ID     string            `yaml:"id"`
	On     []string          `yaml:"on"`
	Preset string            `yaml:"preset"`
	Args   map[string]string `yaml:"args"`
	Jobs   []jobDoc          `yaml:"jobs"`
}

type jobDoc struct {
	ID               string     `yaml:"id"`
	Needs            []string   `yaml:"needs"`
	RunsOn           []string   `yaml:"runs_on"`
	Container        string     `yaml:"container"`
	WorkingDirectory string     `yaml:"working_directory"`
	Steps            []stepDoc  `yaml:"steps"`
	Produce          produceDoc `yaml:"produce"`
}

type stepDoc struct {
	Name             string            `yaml:"name"`
	Run              string            `yaml:"run"`
	Env              map[string]string `yaml:"env"`
	WorkingDirectory string            `yaml:"working_directory"`
}

type produceDoc struct {
	Action        string `yaml:"action"`
	Context       string `yaml:"context"`
	Dockerfile    string `yaml:"dockerfile"`
	Tag           string `yaml:"tag"`
	Path          string `yaml:"path"`
	Destination   string `yaml:"destination"`
	Ref           string `yaml:"ref"`
	Protocol      string `yaml:"protocol"`
	Name          string `yaml:"name"`
	Version       string `yaml:"version"`
	File          string `yaml:"file"`
	Containerfile string `yaml:"containerfile"`
}

// ParseWorkflowFile parses `.easylab/workflows.yaml` into Workflows WITHOUT
// org/repo/branch (the caller sets those from the file's location). A `name`
// filter selects one workflow; empty selects all. Unknown names are returned
// in `missing` for diagnostics.
func ParseWorkflowFile(data []byte, name string) (wfs []*Workflow, missing []string, err error) {
	var f workflowFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("parse workflows.yaml: %w", err)
	}
	if f.Version != 0 && f.Version != 1 {
		return nil, nil, fmt.Errorf("unsupported workflows.yaml version %d", f.Version)
	}
	if len(f.Workflows) == 0 {
		return nil, nil, fmt.Errorf("workflows.yaml declares no workflows")
	}
	want := map[string]bool{}
	if name != "" {
		want[name] = true
	}
	seen := map[string]bool{}
	for i := range f.Workflows {
		d := &f.Workflows[i]
		if d.Name == "" {
			return nil, nil, fmt.Errorf("workflows[%d]: name required", i)
		}
		if seen[d.Name] {
			return nil, nil, fmt.Errorf("duplicate workflow name %q", d.Name)
		}
		seen[d.Name] = true
		if name != "" && d.Name != name {
			continue
		}
		wf, perr := d.workflow()
		if perr != nil {
			return nil, nil, perr
		}
		wfs = append(wfs, wf)
	}
	if name != "" && len(wfs) == 0 {
		missing = append(missing, name)
	}
	return wfs, missing, nil
}

func (d *workflowDoc) workflow() (*Workflow, error) {
	wf := &Workflow{ID: d.ID, Name: d.Name, On: d.On}
	if len(wf.On) == 0 {
		wf.On = []string{"manual"}
	}
	if d.Preset != "" && len(d.Jobs) > 0 {
		return nil, fmt.Errorf("workflow %q: preset and jobs are mutually exclusive", d.Name)
	}
	switch {
	case d.Preset != "":
		job, err := ExpandPreset(d.Preset, "", d.Args)
		if err != nil {
			return nil, fmt.Errorf("workflow %q: %w", d.Name, err)
		}
		wf.Jobs = []Job{job}
	case len(d.Jobs) > 0:
		for i := range d.Jobs {
			j := &d.Jobs[i]
			if j.ID == "" {
				return nil, fmt.Errorf("workflow %q: jobs[%d].id required", d.Name, i)
			}
			wf.Jobs = append(wf.Jobs, j.job())
		}
	default:
		return nil, fmt.Errorf("workflow %q: preset or jobs required", d.Name)
	}
	return wf, nil
}

func (j *jobDoc) job() Job {
	steps := make([]Step, 0, len(j.Steps))
	for _, s := range j.Steps {
		steps = append(steps, Step{Name: s.Name, Run: s.Run, Env: s.Env, WorkingDirectory: s.WorkingDirectory})
	}
	return Job{
		ID: j.ID, Needs: j.Needs, RunsOn: j.RunsOn, Container: j.Container,
		WorkingDirectory: j.WorkingDirectory, Steps: steps,
		Produce: Produce{
			Action: Action(j.Produce.Action), Context: j.Produce.Context,
			Dockerfile: j.Produce.Dockerfile, Tag: j.Produce.Tag,
			Path: j.Produce.Path, Destination: j.Produce.Destination,
			Ref: j.Produce.Ref, Protocol: j.Produce.Protocol,
			Name: j.Produce.Name, Version: j.Produce.Version, File: j.Produce.File,
			Containerfile: j.Produce.Containerfile,
		},
	}
}
