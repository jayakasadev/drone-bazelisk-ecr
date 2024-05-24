package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecr/ecriface"
	"github.com/kelseyhightower/envconfig"
)

// plugin configuraion
type plugin struct {
	Target             string `required:"true"`
	Registry           string `required:"true"`
	CreateRepository   bool   `split_words:"true"`
	Repository         string
	Tag                string
	AccessKey          string `split_words:"true"`
	SecretKey          string `split_words:"true"`
	Bazelrc            string
	Command            string
	CommandArgs        string `split_words:"true"`
	EngflowBesKeywords bool `split_words:"true"`
	TargetArgs         string `split_words:"true"`
}

// plugin constructor
func newPlugin() plugin {
	return plugin{}
}

// process plugin env vars
func (p *plugin) setenv() error {
	err := envconfig.Process("plugin", p)
	if err != nil {
		return err
	}

	// convenience variables to be read by bazel workspace status scripts
	if p.Registry != "" {
		setEnvWithPrefix("REGISTRY", p.Registry)
	}
	if p.Repository != "" {
		setEnvWithPrefix("REPOSITORY", p.Repository)
	}
	if p.Tag != "" {
		setEnvWithPrefix("TAG", p.Tag)
	}

	// setup the credentials used by the amazon-ecr-credential-helper
	if p.AccessKey != "" && p.SecretKey != "" {
		os.Setenv("AWS_ACCESS_KEY_ID", p.AccessKey)
		os.Setenv("AWS_SECRET_ACCESS_KEY", p.SecretKey)
	}

	return nil
}

type buildGetter interface {
	PipelineName() string
	JobName() string
	Uri() string
	ScmRemote() string
	ScmBranch() string
	ScmRevision() string
}

type buildEnv struct{}

func newBuildEnv() *buildEnv {
	return &buildEnv{}
}

func (s *buildEnv) PipelineName() string {
	return os.Getenv("DRONE_STAGE_NAME")
}

func (s *buildEnv) JobName() string {
	return os.Getenv("DRONE_STEP_NAME")
}

func (s *buildEnv) Uri() string {
	return os.Getenv("DRONE_BUILD_LINK")
}

func (s *buildEnv) ScmRemote() string {
	return os.Getenv("DRONE_REPO_LINK")
}

func (s *buildEnv) ScmBranch() string {
	return os.Getenv("DRONE_COMMIT_BRANCH")
}

func (s *buildEnv) ScmRevision() string {
	return os.Getenv("DRONE_COMMIT")
}

func (p *plugin) getArgs(getter buildGetter) []string {
	var args []string

	// append startup options
	if p.Bazelrc != "" {
		args = append(args, joinFlag("--bazelrc", p.Bazelrc))
	}
	command := "run"
	if p.Command != "" {
		command = p.Command
	}

	args = append(args, command)

	// Include Drone CI info for EngFlow
	if p.EngflowBesKeywords {
		args = append(args,
			"--bes_keywords=engflow:CiCdPipelineName="+getter.PipelineName(),
			"--bes_keywords=engflow:CiCdJobName="+getter.JobName(),
			"--bes_keywords=engflow:CiCdUri="+getter.Uri(),
			"--bes_keywords=engflow:BuildScmRemote="+getter.ScmRemote(),
			"--bes_keywords=engflow:BuildScmBranch="+getter.ScmBranch(),
			"--bes_keywords=engflow:BuildScmRevision="+getter.ScmRevision(),
		)
	}

	// append run and target
	if p.CommandArgs != "" {
		args = append(args, p.CommandArgs, p.Target)
	} else {
		args = append(args, p.Target)
	}

	if p.TargetArgs != "" {
		args = append(args, "--", p.TargetArgs)
	}

	return args
}

func (p *plugin) createRepository(svc ecriface.ECRAPI) error {
	// ensure a repository name was provided
	if p.Repository == "" {
		return fmt.Errorf("must specify a repository")
	}

	// get target registry URL from auth token
	result, err := svc.GetAuthorizationToken(&ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return err
	}

	url := aws.StringValue(result.AuthorizationData[0].ProxyEndpoint)
	targetRegistry := strings.TrimPrefix(url, "https://")

	// check that the provided credentials are for the specified registry
	if p.Registry != targetRegistry {
		return fmt.Errorf("provided credentials are not for the specified registry: %s", p.Registry)
	}

	// create repository
	input := &ecr.CreateRepositoryInput{}
	input.SetRepositoryName(p.Repository)
	_, err = svc.CreateRepository(input)
	if err != nil {
		aerr, ok := err.(awserr.Error)
		// ignore repo exists error
		if ok && aerr.Code() == ecr.ErrCodeRepositoryAlreadyExistsException {
			return nil
		}
		return err
	}

	return nil
}

// runs the bazel command
func (p *plugin) run() error {
	err := p.setenv()
	if err != nil {
		return err
	}

	if p.CreateRepository {
		svc, err := p.ecrClient()
		if err != nil {
			return err
		}

		err = p.createRepository(svc)
		if err != nil {
			return err
		}
	}

	// exec bazel
	cmd := exec.Command("bazel", p.getArgs(newBuildEnv())...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// parse AWS region from registry URL
func (p *plugin) region() (string, error) {
	splitRegistry := strings.Split(p.Registry, ".")

	// avoid index out of bounds
	if len(splitRegistry) < 4 {
		return "", fmt.Errorf("could not parse region from registry: %s", p.Registry)
	}

	return splitRegistry[3], nil
}

// get an ecr service client
func (p *plugin) ecrClient() (*ecr.ECR, error) {
	region, err := p.region()
	if err != nil {
		return nil, err
	}

	config := aws.NewConfig().WithRegion(region)
	return ecr.New(session.New(), config), nil
}

func setEnvWithPrefix(key, val string) {
	os.Setenv(fmt.Sprintf("%s_%s", "DRONE_ECR", key), val)
}

func joinFlag(flag, value string) string {
	return fmt.Sprintf("%s=%s", flag, value)
}
