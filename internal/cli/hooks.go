package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

const (
	hostHookPostCreate = "postCreate"
	hostHookPostStart  = "postStart"
	hostHookPreRemove  = "preRemove"
	hostHookPostRemove = "postRemove"
)

type hostHookContext struct {
	Hook          string
	CommandName   string
	ContainerName string
	ImageName     string
	ImageTag      string
	Workdir       string
	HostPort      int
	ContainerPort int
	ConfigDir     string
	DataDir       string
}

func startContainerWithPostStartHook(cmd *cobra.Command, cfg config.Config, configDir, name, commandName string) error {
	if err := docker.Start(name); err != nil {
		return err
	}
	if commandName == "" && cmd != nil {
		commandName = cmd.Name()
	}
	return runHostHooksForContainer(cmd, cfg, hostHookPostStart, hostHookContext{
		CommandName:   commandName,
		ContainerName: name,
		ConfigDir:     configDir,
	})
}

func runHostHooksForContainer(cmd *cobra.Command, cfg config.Config, hookName string, ctx hostHookContext) error {
	ctx = enrichHostHookContextForContainer(cfg, hookName, ctx)
	if strings.TrimSpace(ctx.ContainerName) == "" {
		return nil
	}
	if isTruthyEnv("DV_NO_HOOKS") || len(configuredHooksForName(cfg.Hooks, hookName)) == 0 {
		return nil
	}

	return runConfiguredHostHooks(cmd, cfg, hookName, ctx)
}

func enrichHostHookContextForContainer(cfg config.Config, hookName string, ctx hostHookContext) hostHookContext {
	ctx.Hook = hookName
	if strings.TrimSpace(ctx.ContainerName) == "" {
		return ctx
	}

	labels, _ := labelsWithOverrides(ctx.ContainerName, cfg)
	if ctx.ImageName == "" {
		ctx.ImageName = labels["com.dv.image-name"]
	}
	if ctx.ImageTag == "" {
		ctx.ImageTag = labels["com.dv.image-tag"]
	}

	var imgCfg config.ImageConfig
	if ctx.ImageName != "" {
		imgCfg = cfg.Images[ctx.ImageName]
	}
	if ctx.ImageTag == "" {
		ctx.ImageTag = imgCfg.Tag
	}
	if ctx.Workdir == "" {
		if workdir, err := docker.GetContainerWorkdir(ctx.ContainerName); err == nil {
			ctx.Workdir = workdir
		}
	}
	if ctx.Workdir == "" {
		ctx.Workdir = config.EffectiveWorkdir(cfg, imgCfg, ctx.ContainerName)
	}
	if ctx.ContainerPort == 0 {
		ctx.ContainerPort = imgCfg.ContainerPort
	}
	if ctx.ContainerPort == 0 {
		ctx.ContainerPort = cfg.ContainerPort
	}
	if ctx.HostPort == 0 && ctx.ContainerPort > 0 {
		if hostPort, err := docker.GetContainerHostPort(ctx.ContainerName, ctx.ContainerPort); err == nil {
			ctx.HostPort = hostPort
		}
	}

	return ctx
}

func runConfiguredHostHooks(cmd *cobra.Command, cfg config.Config, hookName string, ctx hostHookContext) error {
	if isTruthyEnv("DV_NO_HOOKS") {
		return nil
	}

	hooks := configuredHooksForName(cfg.Hooks, hookName)
	if len(hooks) == 0 {
		return nil
	}

	ctx.Hook = hookName
	if ctx.CommandName == "" && cmd != nil {
		ctx.CommandName = cmd.Name()
	}
	if ctx.ConfigDir == "" {
		if configDir, err := xdg.ConfigDir(); err == nil {
			ctx.ConfigDir = configDir
		}
	}
	if ctx.DataDir == "" {
		if dataDir, err := xdg.DataDir(); err == nil {
			ctx.DataDir = dataDir
		}
	}

	out := hookOutput(cmd)
	errOut := hookErrOutput(cmd)
	in := hookInput(cmd)

	runnableHooks := runnableHostHooks(hooks)
	for i, hook := range runnableHooks {
		fmt.Fprintf(out, "Running %s hook %d/%d...\n", hookName, i+1, len(runnableHooks))
		if isTruthyEnv("DV_VERBOSE") {
			fmt.Fprintf(out, "Hook command: %s\n", hook.Command)
		}

		if err := runHostHookCommand(in, out, errOut, hook, ctx, i); err != nil {
			if hook.IgnoreErrors {
				fmt.Fprintf(errOut, "Warning: %s hook failed: %v\n", hookName, err)
				continue
			}
			return fmt.Errorf("%s hook failed: %w", hookName, err)
		}
	}

	return nil
}

func configuredHooksForName(hooks config.HooksConfig, hookName string) []config.HostHook {
	switch hookName {
	case hostHookPostCreate:
		return hooks.PostCreate
	case hostHookPostStart:
		return hooks.PostStart
	case hostHookPreRemove:
		return hooks.PreRemove
	case hostHookPostRemove:
		return hooks.PostRemove
	default:
		return nil
	}
}

func runnableHostHooks(hooks []config.HostHook) []config.HostHook {
	runnable := make([]config.HostHook, 0, len(hooks))
	for _, hook := range hooks {
		if hook.Enabled != nil && !*hook.Enabled {
			continue
		}
		if strings.TrimSpace(hook.Command) == "" {
			continue
		}
		runnable = append(runnable, hook)
	}
	return runnable
}

func runHostHookCommand(in io.Reader, out, errOut io.Writer, hook config.HostHook, ctx hostHookContext, index int) error {
	cmdCtx := context.Background()
	var cancel context.CancelFunc = func() {}
	if hook.TimeoutSeconds > 0 {
		cmdCtx, cancel = context.WithTimeout(cmdCtx, time.Duration(hook.TimeoutSeconds)*time.Second)
	}
	defer cancel()

	args := append([]string{"-c", hook.Command, "dv-hook"}, hostHookArgs(ctx)...)
	hookCmd := exec.CommandContext(cmdCtx, "/bin/sh", args...)
	hookCmd.Stdin = in
	hookCmd.Stdout = out
	hookCmd.Stderr = errOut
	hookCmd.Env = hostHookEnv(ctx, index)

	err := hookCmd.Run()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %ds", hook.TimeoutSeconds)
	}
	return err
}

func hostHookEnv(ctx hostHookContext, index int) []string {
	hostPort := ""
	if ctx.HostPort > 0 {
		hostPort = strconv.Itoa(ctx.HostPort)
	}
	containerPort := ""
	if ctx.ContainerPort > 0 {
		containerPort = strconv.Itoa(ctx.ContainerPort)
	}

	values := []string{
		"DV_HOOK=" + ctx.Hook,
		"DV_COMMAND=" + ctx.CommandName,
		"DV_CONTAINER_NAME=" + ctx.ContainerName,
		"DV_IMAGE_NAME=" + ctx.ImageName,
		"DV_IMAGE_TAG=" + ctx.ImageTag,
		"DV_WORKDIR=" + ctx.Workdir,
		"DV_HOST_PORT=" + hostPort,
		"DV_CONTAINER_PORT=" + containerPort,
		"DV_CONFIG_DIR=" + ctx.ConfigDir,
		"DV_DATA_DIR=" + ctx.DataDir,
		"DV_HOOK_INDEX=" + strconv.Itoa(index),
		"DV_NO_HOOKS=1",
	}
	keys := map[string]struct{}{}
	for _, entry := range values {
		key, _, _ := strings.Cut(entry, "=")
		keys[key] = struct{}{}
	}

	env := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		if _, overwrite := keys[key]; overwrite {
			continue
		}
		env = append(env, entry)
	}
	return append(env, values...)
}

func hostHookArgs(ctx hostHookContext) []string {
	hostPort := ""
	if ctx.HostPort > 0 {
		hostPort = strconv.Itoa(ctx.HostPort)
	}
	containerPort := ""
	if ctx.ContainerPort > 0 {
		containerPort = strconv.Itoa(ctx.ContainerPort)
	}
	return []string{
		ctx.ContainerName,
		hostPort,
		containerPort,
		ctx.Hook,
		ctx.ImageName,
		ctx.ImageTag,
		ctx.Workdir,
	}
}

func newHostHookCommand(name string, in io.Reader, out, errOut io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: name}
	if in == nil {
		in = strings.NewReader("")
	}
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	cmd.SetIn(in)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd
}

func hookOutput(cmd *cobra.Command) io.Writer {
	if cmd == nil {
		return os.Stdout
	}
	return cmd.OutOrStdout()
}

func hookErrOutput(cmd *cobra.Command) io.Writer {
	if cmd == nil {
		return os.Stderr
	}
	return cmd.ErrOrStderr()
}

func hookInput(cmd *cobra.Command) io.Reader {
	if cmd == nil {
		return os.Stdin
	}
	return cmd.InOrStdin()
}
