package terraform

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/kballard/go-shellquote"

	"github.com/infracost/infracost/internal/config"
	"github.com/infracost/infracost/internal/schema"
	"github.com/infracost/infracost/internal/ui"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

var defaultTerragruntBinary = "terragrunt"
var minTerragruntVer = "v0.28.1"

type TerragruntProvider struct {
	ctx             *config.ProjectContext
	Path            string
	TerragruntFlags string
	*DirProvider
}

type TerragruntInfo struct {
	ConfigPath string
	WorkingDir string
}

type terragruntProjectDirs struct {
	ConfigDir  string
	WorkingDir string
}

func NewTerragruntProvider(ctx *config.ProjectContext) schema.Provider {
	dirProvider := NewDirProvider(ctx).(*DirProvider)

	terragruntBinary := ctx.ProjectConfig.TerraformBinary
	if terragruntBinary == "" {
		terragruntBinary = defaultTerragruntBinary
	}

	dirProvider.TerraformBinary = terragruntBinary
	dirProvider.IsTerragrunt = true

	return &TerragruntProvider{
		ctx:             ctx,
		DirProvider:     dirProvider,
		Path:            ctx.ProjectConfig.Path,
		TerragruntFlags: ctx.ProjectConfig.TerragruntFlags,
	}
}

func (p *TerragruntProvider) Type() string {
	return "terragrunt"
}

func (p *TerragruntProvider) DisplayType() string {
	return "Terragrunt directory"
}

func (p *TerragruntProvider) AddMetadata(metadata *schema.ProjectMetadata) {
	// no op
}

func (p *TerragruntProvider) LoadResources(usage map[string]*schema.UsageData) ([]*schema.Project, error) {
	// We want to run Terragrunt commands from the config dirs
	// Terragrunt internally runs Terraform in the working dirs, so we need to be aware of these
	// so we can handle reading and cleaning up the generated plan files.
	projectDirs, err := p.getProjectDirs()

	if err != nil {
		return []*schema.Project{}, err
	}

	var outs [][]byte

	if p.UseState {
		outs, err = p.generateStateJSONs(projectDirs)
	} else {
		outs, err = p.generatePlanJSONs(projectDirs)
	}
	if err != nil {
		return []*schema.Project{}, err
	}

	projects := make([]*schema.Project, 0, len(projectDirs))

	for i, projectDir := range projectDirs {
		metadata := config.DetectProjectMetadata(projectDir.ConfigDir)
		metadata.Type = p.Type()
		p.AddMetadata(metadata)
		name := schema.GenerateProjectName(metadata, p.ctx.RunContext.Config.EnableDashboard)

		project := schema.NewProject(name, metadata)

		parser := NewParser(p.ctx)
		pastResources, resources, err := parser.parseJSON(outs[i], usage)
		if err != nil {
			return projects, errors.Wrap(err, "Error parsing Terraform JSON")
		}

		project.HasDiff = !p.UseState
		if project.HasDiff {
			project.PastResources = pastResources
		}
		project.Resources = resources

		projects = append(projects, project)
	}

	return projects, nil
}

func (p *TerragruntProvider) getProjectDirs() ([]terragruntProjectDirs, error) {
	spinner := ui.NewSpinner("Running terragrunt run-all terragrunt-info", p.spinnerOpts)
	defer spinner.Fail()

	terragruntFlags, err := shellquote.Split(p.TerragruntFlags)
	if err != nil {
		return []terragruntProjectDirs{}, errors.Wrap(err, "Error parsing terragrunt flags")
	}

	opts := &CmdOptions{
		TerraformBinary: p.TerraformBinary,
		Dir:             p.Path,
		Flags:           terragruntFlags,
	}
	out, err := Cmd(opts, "run-all", "--terragrunt-ignore-external-dependencies", "terragrunt-info")
	if err != nil {
		spinner.Fail()
		err = p.buildTerraformErr(err)

		return []terragruntProjectDirs{}, err
	}

	var jsons [][]byte

	jsonStart := bytes.IndexByte(out, '{') // ignore anything that comes before the json (e.g. unexpected logging to stdout by tgenv)
	if jsonStart >= 0 {
		out = out[jsonStart:]

		jsons = bytes.SplitAfter(out, []byte{'}', '\n'})
		if len(jsons) > 1 {
			jsons = jsons[:len(jsons)-1]
		}
	}

	dirs := make([]terragruntProjectDirs, 0, len(jsons))

	for _, j := range jsons {
		var info TerragruntInfo
		err = json.Unmarshal(j, &info)
		if err != nil {
			spinner.Fail()
			return dirs, err
		}

		dirs = append(dirs, terragruntProjectDirs{
			ConfigDir:  filepath.Dir(info.ConfigPath),
			WorkingDir: info.WorkingDir,
		})
	}

	// Sort the dirs so they are consistent in the output
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].ConfigDir < dirs[j].ConfigDir
	})

	spinner.Success()

	return dirs, nil
}

func (p *TerragruntProvider) generateStateJSONs(projectDirs []terragruntProjectDirs) ([][]byte, error) {
	err := p.checks()
	if err != nil {
		return [][]byte{}, err
	}

	outs := make([][]byte, 0, len(projectDirs))

	spinnerMsg := "Running terragrunt show"
	if len(projectDirs) > 1 {
		spinnerMsg += " for each project"
	}
	spinner := ui.NewSpinner(spinnerMsg, p.spinnerOpts)
	defer spinner.Fail()

	for _, projectDir := range projectDirs {
		opts, err := p.buildCommandOpts(projectDir.ConfigDir)
		if err != nil {
			return [][]byte{}, err
		}

		terragruntFlags, err := shellquote.Split(p.TerragruntFlags)
		if err != nil {
			return [][]byte{}, errors.Wrap(err, "Error parsing terragrunt flags")
		}
		opts.Flags = terragruntFlags

		if opts.TerraformConfigFile != "" {
			defer os.Remove(opts.TerraformConfigFile)
		}

		out, err := p.runShow(opts, spinner, "")
		if err != nil {
			return outs, err
		}

		// ignore anything that comes before the json (e.g. unexpected logging to stdout by tgenv)
		jsonStart := bytes.IndexByte(out, '{')
		if jsonStart >= 0 {
			out = out[jsonStart:]
		}

		outs = append(outs, out)
	}

	return outs, nil
}

func (p *TerragruntProvider) generatePlanJSONs(projectDirs []terragruntProjectDirs) ([][]byte, error) {
	err := p.checks()
	if err != nil {
		return [][]byte{}, err
	}

	opts, err := p.buildCommandOpts(p.Path)
	if err != nil {
		return [][]byte{}, err
	}

	terragruntFlags, err := shellquote.Split(p.TerragruntFlags)
	if err != nil {
		return [][]byte{}, errors.Wrap(err, "Error parsing terragrunt flags")
	}
	opts.Flags = terragruntFlags

	if opts.TerraformConfigFile != "" {
		defer os.Remove(opts.TerraformConfigFile)
	}

	spinner := ui.NewSpinner("Running terragrunt run-all plan", p.spinnerOpts)
	defer spinner.Fail()

	planFile, planJSON, err := p.runPlan(opts, spinner, true)
	defer func() {
		err := cleanupPlanFiles(projectDirs, planFile)
		if err != nil {
			log.Warnf("Error cleaning up plan files: %v", err)
		}
	}()

	if err != nil {
		return [][]byte{}, err
	}

	if len(planJSON) > 0 {
		return [][]byte{planJSON}, nil
	}

	outs := make([][]byte, 0, len(projectDirs))
	spinnerMsg := "Running terragrunt show"
	if len(projectDirs) > 1 {
		spinnerMsg += " for each project"
	}
	spinner = ui.NewSpinner(spinnerMsg, p.spinnerOpts)

	for _, projectDir := range projectDirs {
		opts, err := p.buildCommandOpts(projectDir.ConfigDir)
		if err != nil {
			return [][]byte{}, err
		}
		if opts.TerraformConfigFile != "" {
			defer os.Remove(opts.TerraformConfigFile)
		}

		out, err := p.runShow(opts, spinner, filepath.Join(projectDir.WorkingDir, planFile))
		if err != nil {
			return outs, err
		}

		// ignore anything that comes before the json (e.g. unexpected logging to stdout by tgenv)
		jsonStart := bytes.IndexByte(out, '{')
		if jsonStart >= 0 {
			out = out[jsonStart:]
		}

		outs = append(outs, out)
	}

	return outs, nil
}

func cleanupPlanFiles(projectDirs []terragruntProjectDirs, planFile string) error {
	if planFile == "" {
		return nil
	}

	for _, projectDir := range projectDirs {
		err := os.Remove(filepath.Join(projectDir.WorkingDir, planFile))
		if err != nil {
			return err
		}
	}

	return nil
}
