package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/assets"
	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/localproxy"
	"dv/internal/xdg"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run a dv HTTP API server",
	RunE: func(cmd *cobra.Command, args []string) error {
		host, _ := cmd.Flags().GetString("host")
		port, _ := cmd.Flags().GetInt("port")
		overrideToken, _ := cmd.Flags().GetString("token")

		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		activeToken, generated, err := ensureServeToken(&cfg, configDir, overrideToken)
		if err != nil {
			return err
		}
		if generated {
			fmt.Fprintf(cmd.OutOrStdout(), "Generated dv serve token: %s\n", activeToken)
		}

		handler := authMiddleware(activeToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleServeRequest(w, r, configDir)
		}))

		srv := &http.Server{
			Addr:    fmt.Sprintf("%s:%d", host, port),
			Handler: handler,
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- srv.ListenAndServe()
		}()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		select {
		case sig := <-sigCh:
			fmt.Fprintf(cmd.OutOrStdout(), "Shutting down dv serve (%s)...\n", sig.String())
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	},
}

func init() {
	serveCmd.Flags().Int("port", 7373, "Port to listen on")
	serveCmd.Flags().String("host", "127.0.0.1", "Host to bind to")
	serveCmd.Flags().String("token", "", "Bearer token to require")
}

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
}

func (s *sseWriter) writeEvent(event string, data interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = writeSSE(s.w, event, data)
	s.flusher.Flush()
}

func (s *sseWriter) writeComment(comment string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, ": %s\n\n", comment)
	s.flusher.Flush()
}

func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")) != token {
			writeJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func ensureServeToken(cfg *config.Config, configDir, override string) (string, bool, error) {
	if strings.TrimSpace(override) != "" {
		cfg.ServeToken = override
		if err := config.Save(configDir, *cfg); err != nil {
			return "", false, err
		}
		return override, false, nil
	}
	if strings.TrimSpace(cfg.ServeToken) != "" {
		return cfg.ServeToken, false, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, err
	}
	token := hex.EncodeToString(buf)
	cfg.ServeToken = token
	if err := config.Save(configDir, *cfg); err != nil {
		return "", false, err
	}
	return token, true, nil
}

func handleServeRequest(w http.ResponseWriter, r *http.Request, configDir string) {
	path := strings.Trim(strings.TrimSpace(r.URL.Path), "/")
	switch {
	case r.Method == http.MethodGet && path == "status":
		handleStatus(w, r, configDir)
		return
	case path == "containers":
		handleContainers(w, r, configDir)
		return
	case strings.HasPrefix(path, "containers/"):
		handleContainer(w, r, configDir, strings.Split(path, "/"))
		return
	case path == "images":
		handleImages(w, r, configDir)
		return
	case strings.HasPrefix(path, "images/"):
		handleImageActions(w, r, configDir, strings.Split(path, "/"))
		return
	case path == "config":
		handleConfig(w, r, configDir)
		return
	default:
		writeJSON(w, http.StatusNotFound, "not found")
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request, configDir string) {
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	selected := strings.TrimSpace(cfg.SelectedAgent)
	if selected == "" {
		selected = cfg.DefaultContainer
	}
	payload := map[string]interface{}{
		"version":            version,
		"selected_container": selected,
		"selected_image":     cfg.SelectedImage,
	}
	writeJSON(w, http.StatusOK, payload)
}

func handleContainers(w http.ResponseWriter, r *http.Request, configDir string) {
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	switch r.Method {
	case http.MethodGet:
		withSessions := strings.EqualFold(r.URL.Query().Get("sessions"), "true")
		containers, selected, err := listContainers(cfg, withSessions)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"containers": containers,
			"selected":   selected,
		})
	case http.MethodPost:
		handleContainerCreate(w, r, configDir, cfg)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleContainerCreate(w http.ResponseWriter, r *http.Request, configDir string, cfg config.Config) {
	var req struct {
		Name             string `json:"name"`
		Image            string `json:"image"`
		HostStartingPort int    `json:"host_starting_port"`
		ContainerPort    int    `json:"container_port"`
		Reset            bool   `json:"reset"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = currentAgentName(cfg)
	}
	if name == "" {
		writeJSON(w, http.StatusBadRequest, "container name required")
		return
	}

	hostPort := req.HostStartingPort
	if hostPort == 0 {
		hostPort = cfg.HostStartingPort
	}

	imgName, imgCfg, err := resolveImage(cfg, req.Image)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	containerPort := req.ContainerPort
	if containerPort == 0 {
		containerPort = imgCfg.ContainerPort
	}
	if containerPort == 0 {
		containerPort = cfg.ContainerPort
	}
	workdir := imgCfg.Workdir
	if strings.TrimSpace(workdir) == "" {
		workdir = "/var/www/discourse"
	}

	streamExec(w, func(stdout, stderr io.Writer) error {
		logger := func(line string) {
			fmt.Fprint(stdout, line)
		}
		if req.Reset && docker.Exists(name) {
			logger(fmt.Sprintf("Stopping and removing container '%s'...\n", name))
			_ = docker.Stop(name)
			_ = docker.Remove(name)
		}
		if !docker.Exists(name) {
			allocated, _ := docker.AllocatedPorts()
			chosenPort := hostPort
			for isPortInUse(chosenPort, allocated) {
				chosenPort++
			}
			if chosenPort != hostPort {
				logger(fmt.Sprintf("Port %d in use, using %d.\n", hostPort, chosenPort))
			}
			labels := map[string]string{
				"com.dv.owner":      "dv",
				"com.dv.image-name": imgName,
				"com.dv.image-tag":  imgCfg.Tag,
			}
			envs := map[string]string{
				"DISCOURSE_PORT": strconv.Itoa(chosenPort),
			}
			logger(fmt.Sprintf("Creating and starting container '%s' with image '%s'...\n", name, imgCfg.Tag))
			if err := docker.RunDetached(name, workdir, imgCfg.Tag, chosenPort, containerPort, labels, envs, nil, ""); err != nil {
				return err
			}
		} else if !docker.Running(name) {
			logger(fmt.Sprintf("Starting existing container '%s'...\n", name))
			if err := docker.Start(name); err != nil {
				return err
			}
		} else {
			logger(fmt.Sprintf("Container '%s' is already running.\n", name))
		}

		if cfg.ContainerImages == nil {
			cfg.ContainerImages = map[string]string{}
		}
		cfg.ContainerImages[name] = imgName
		_ = config.Save(configDir, cfg)
		return nil
	}, true)
}

func handleContainer(w http.ResponseWriter, r *http.Request, configDir string, parts []string) {
	if len(parts) < 2 {
		writeJSON(w, http.StatusNotFound, "not found")
		return
	}
	name := parts[1]
	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			handleContainerInfo(w, r, configDir, name)
		case http.MethodDelete:
			handleContainerDelete(w, r, configDir, name)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if len(parts) >= 3 {
		action := parts[2]
		switch action {
		case "start":
			handleContainerStart(w, r, configDir, name)
		case "stop":
			handleContainerStop(w, r, name)
		case "restart":
			handleContainerRestart(w, r, name)
		case "select":
			handleContainerSelect(w, r, configDir, name)
		case "rename":
			handleContainerRename(w, r, configDir, name)
		case "run":
			handleContainerRun(w, r, configDir, name)
		case "run-agent":
			handleContainerRunAgent(w, r, configDir, name)
		case "extract":
			handleContainerExtract(w, r, name)
		case "branch":
			handleContainerBranch(w, r, configDir, name)
		case "catchup":
			handleContainerCatchup(w, r, configDir, name)
		case "reset":
			handleContainerReset(w, r, configDir, name)
		case "ps":
			handleContainerPS(w, r, name)
		case "update":
			if len(parts) >= 4 && parts[3] == "agents" {
				handleContainerUpdateAgents(w, r, configDir, name)
				return
			}
			writeJSON(w, http.StatusNotFound, "not found")
		case "logs":
			if len(parts) >= 4 {
				switch parts[3] {
				case "unicorn":
					handleContainerLogTail(w, r, name, "/var/www/discourse/log/unicorn.log")
				case "ember":
					handleContainerLogTail(w, r, name, "/var/www/discourse/log/ember-cli.log")
				default:
					writeJSON(w, http.StatusNotFound, "not found")
				}
				return
			}
			writeJSON(w, http.StatusNotFound, "not found")
		default:
			writeJSON(w, http.StatusNotFound, "not found")
		}
		return
	}
	writeJSON(w, http.StatusNotFound, "not found")
}

func handleContainerInfo(w http.ResponseWriter, r *http.Request, configDir, name string) {
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	containers, selected, err := listContainers(cfg, false)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, c := range containers {
		if c["name"] == name {
			writeJSON(w, http.StatusOK, c)
			return
		}
	}
	_ = selected
	writeJSON(w, http.StatusNotFound, "container not found")
}

func handleContainerStart(w http.ResponseWriter, r *http.Request, configDir, name string) {
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req struct {
		Reset bool `json:"reset"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	imgName := cfg.ContainerImages[name]
	if imgName == "" {
		imgName = cfg.SelectedImage
	}
	imgCfg, ok := cfg.Images[imgName]
	if !ok {
		writeJSON(w, http.StatusBadRequest, "unknown image")
		return
	}
	workdir := imgCfg.Workdir
	if strings.TrimSpace(workdir) == "" {
		workdir = "/var/www/discourse"
	}

	streamExec(w, func(stdout, stderr io.Writer) error {
		logger := func(line string) { fmt.Fprint(stdout, line) }
		if req.Reset && docker.Exists(name) {
			logger(fmt.Sprintf("Resetting container '%s'...\n", name))
			_ = docker.Stop(name)
			_ = docker.Remove(name)
		}
		if !docker.Exists(name) {
			allocated, _ := docker.AllocatedPorts()
			chosenPort := cfg.HostStartingPort
			for isPortInUse(chosenPort, allocated) {
				chosenPort++
			}
			labels := map[string]string{
				"com.dv.owner":      "dv",
				"com.dv.image-name": imgName,
				"com.dv.image-tag":  imgCfg.Tag,
			}
			envs := map[string]string{
				"DISCOURSE_PORT": strconv.Itoa(chosenPort),
			}
			logger(fmt.Sprintf("Creating and starting container '%s'...\n", name))
			return docker.RunDetached(name, workdir, imgCfg.Tag, chosenPort, cfg.ContainerPort, labels, envs, nil, "")
		}
		if !docker.Running(name) {
			logger(fmt.Sprintf("Starting container '%s'...\n", name))
			return docker.Start(name)
		}
		logger(fmt.Sprintf("Container '%s' already running.\n", name))
		return nil
	}, true)
}

func handleContainerStop(w http.ResponseWriter, r *http.Request, name string) {
	streamExec(w, func(stdout, stderr io.Writer) error {
		fmt.Fprintf(stdout, "Stopping container '%s'...\n", name)
		if docker.Running(name) {
			return docker.Stop(name)
		}
		fmt.Fprintln(stdout, "Container already stopped.")
		return nil
	}, true)
}

func handleContainerRestart(w http.ResponseWriter, r *http.Request, name string) {
	streamExec(w, func(stdout, stderr io.Writer) error {
		if docker.Running(name) {
			fmt.Fprintf(stdout, "Stopping container '%s'...\n", name)
			if err := docker.Stop(name); err != nil {
				return err
			}
		}
		fmt.Fprintf(stdout, "Starting container '%s'...\n", name)
		return docker.Start(name)
	}, true)
}

func handleContainerDelete(w http.ResponseWriter, r *http.Request, configDir, name string) {
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req struct {
		RemoveImage bool `json:"remove_image"`
		Force       bool `json:"force"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if docker.Exists(name) && !req.Force {
		sessions, err := docker.ExecSessions(name)
		if err == nil && len(sessions) > 0 {
			writeJSON(w, http.StatusConflict, "container has active sessions")
			return
		}
	}

	if docker.Exists(name) {
		if docker.Running(name) {
			_ = docker.RemoveForce(name)
		} else {
			_ = docker.Remove(name)
		}
	}

	if req.RemoveImage {
		imageTag := ""
		if imgName := cfg.ContainerImages[name]; imgName != "" {
			if imgCfg, ok := cfg.Images[imgName]; ok {
				imageTag = imgCfg.Tag
			}
		} else {
			imageTag, _ = containerImage(name)
		}
		if imageTag != "" {
			_ = docker.RemoveImage(imageTag)
		}
	}

	if cfg.ContainerImages != nil {
		delete(cfg.ContainerImages, name)
	}
	if cfg.CustomWorkdirs != nil {
		delete(cfg.CustomWorkdirs, name)
	}
	if cfg.SelectedAgent == name {
		cfg.SelectedAgent = ""
	}
	_ = config.Save(configDir, cfg)

	writeJSON(w, http.StatusOK, map[string]interface{}{})
}

func handleContainerSelect(w http.ResponseWriter, r *http.Request, configDir, name string) {
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg.SelectedAgent = name
	if err := config.Save(configDir, cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{})
}

func handleContainerRename(w http.ResponseWriter, r *http.Request, configDir, name string) {
	var req struct {
		NewName string `json:"new_name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	newName := strings.TrimSpace(req.NewName)
	if newName == "" {
		writeJSON(w, http.StatusBadRequest, "new_name required")
		return
	}
	if err := docker.Rename(name, newName); err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.SelectedAgent == name {
		cfg.SelectedAgent = newName
	}
	if cfg.ContainerImages != nil {
		if img, ok := cfg.ContainerImages[name]; ok {
			delete(cfg.ContainerImages, name)
			cfg.ContainerImages[newName] = img
		}
	}
	if cfg.CustomWorkdirs != nil {
		if wdir, ok := cfg.CustomWorkdirs[name]; ok {
			delete(cfg.CustomWorkdirs, name)
			cfg.CustomWorkdirs[newName] = wdir
		}
	}
	_ = config.Save(configDir, cfg)
	writeJSON(w, http.StatusOK, map[string]interface{}{})
}

func handleContainerRun(w http.ResponseWriter, r *http.Request, configDir, name string) {
	var req struct {
		Cmd     string            `json:"cmd"`
		Workdir string            `json:"workdir"`
		AsRoot  bool              `json:"as_root"`
		Env     map[string]string `json:"env"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Cmd) == "" {
		writeJSON(w, http.StatusBadRequest, "cmd required")
		return
	}

	ctx, err := ensureContainerExecContext(configDir, name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	workdir := ctx.workdir
	if strings.TrimSpace(req.Workdir) != "" {
		workdir = req.Workdir
	}

	envs := append(docker.Envs{}, ctx.envs...)
	for k, v := range req.Env {
		if strings.TrimSpace(k) == "" {
			continue
		}
		envs = append(envs, fmt.Sprintf("%s=%s", k, v))
	}

	argv := []string{"bash", "-lc", req.Cmd}
	if req.AsRoot {
		streamExec(w, func(stdout, stderr io.Writer) error {
			return execStreamAsUser("root", name, workdir, envs, argv, stdout, stderr)
		}, true)
		return
	}
	streamExec(w, func(stdout, stderr io.Writer) error {
		return docker.ExecStream(name, workdir, envs, argv, stdout, stderr)
	}, true)
}

func handleContainerRunAgent(w http.ResponseWriter, r *http.Request, configDir, name string) {
	var req struct {
		Agent   string   `json:"agent"`
		Prompt  string   `json:"prompt"`
		RawArgs []string `json:"raw_args"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	agent := strings.TrimSpace(req.Agent)
	if agent == "" {
		writeJSON(w, http.StatusBadRequest, "agent required")
		return
	}
	agent = resolveAgentAlias(agent)

	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, err := ensureContainerExecContext(configDir, name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	workdir := ctx.workdir

	cmdStub := &cobra.Command{}
	cmdStub.SetOut(io.Discard)
	cmdStub.SetErr(io.Discard)
	copyConfiguredFiles(cmdStub, cfg, name, workdir, agent)
	envs := buildAgentEnv(cfg, agent, cmdStub)

	var argv []string
	if len(req.RawArgs) > 0 {
		argv = append([]string{agent}, req.RawArgs...)
	} else if strings.TrimSpace(req.Prompt) == "" {
		argv = buildAgentInteractive(agent)
	} else {
		argv = buildAgentArgs(agent, req.Prompt)
	}

	shellCmd := withUserPaths(shellJoin(argv))
	finalArgs := []string{"bash", "-lc", shellCmd}

	streamExec(w, func(stdout, stderr io.Writer) error {
		return docker.ExecStream(name, workdir, envs, finalArgs, stdout, stderr)
	}, true)
}

func handleContainerExtract(w http.ResponseWriter, r *http.Request, name string) {
	var req struct {
		Path string `json:"path"`
		Dir  string `json:"dir"`
		Sync bool   `json:"sync"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	exe, err := os.Executable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	args := []string{"extract", "--name", name}
	if strings.TrimSpace(req.Dir) != "" {
		args = append(args, "--dir", req.Dir)
	}
	if req.Sync {
		args = append(args, "--sync")
	}
	if strings.TrimSpace(req.Path) != "" {
		args = append(args, req.Path)
	}

	streamHostCommand(w, r.Context(), exe, args, true)
}

func handleContainerBranch(w http.ResponseWriter, r *http.Request, configDir, name string) {
	var req struct {
		Branch  string `json:"branch"`
		NoReset bool   `json:"no_reset"`
		New     bool   `json:"new"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		writeJSON(w, http.StatusBadRequest, "branch required")
		return
	}

	ctx, _, err := ensureDiscourseContainer(configDir, name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	checkoutCmds := buildBranchCheckoutCommands(branch)
	if req.New {
		exists, err := remoteBranchExists("https://github.com/discourse/discourse.git", branch)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !exists {
			checkoutCmds = buildNewBranchCheckoutCommands(branch)
		}
	}
	script := buildDiscourseResetScript(checkoutCmds, discourseResetScriptOpts{SkipDBReset: req.NoReset})
	argv := []string{"bash", "-lc", script}
	workdir := ctx.workdir

	streamExec(w, func(stdout, stderr io.Writer) error {
		return docker.ExecStream(ctx.name, workdir, nil, argv, stdout, stderr)
	}, true)
}

func handleContainerCatchup(w http.ResponseWriter, r *http.Request, configDir, name string) {
	ctx, _, err := ensureDiscourseContainer(configDir, name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	workdir := ctx.workdir

	findScript := "find plugins -maxdepth 2 -name .git -type d 2>/dev/null | sed 's|/.git$||' | sort"
	pluginOutput, err := docker.ExecOutput(ctx.name, workdir, nil, []string{"bash", "-c", findScript})
	if err != nil {
		pluginOutput = ""
	}
	var plugins []string
	for _, line := range strings.Split(strings.TrimSpace(pluginOutput), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			plugins = append(plugins, line)
		}
	}

	script := buildCatchupScript(workdir, plugins)
	argv := []string{"bash", "-lc", script}

	streamExec(w, func(stdout, stderr io.Writer) error {
		return docker.ExecStream(ctx.name, workdir, nil, argv, stdout, stderr)
	}, true)
}

func handleContainerReset(w http.ResponseWriter, r *http.Request, configDir, name string) {
	var req struct {
		DiscourseReset bool `json:"discourse_reset"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, _, err := ensureDiscourseContainer(configDir, name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	workdir := ctx.workdir

	var script string
	if req.DiscourseReset {
		script = buildDiscourseResetScript(buildCurrentBranchResetCommands(), discourseResetScriptOpts{})
	} else {
		script = buildDiscourseDatabaseResetScript()
	}
	argv := []string{"bash", "-lc", script}

	streamExec(w, func(stdout, stderr io.Writer) error {
		return docker.ExecStream(ctx.name, workdir, nil, argv, stdout, stderr)
	}, true)
}

func handleContainerPS(w http.ResponseWriter, r *http.Request, name string) {
	sessions, err := docker.ExecSessions(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, map[string]interface{}{
			"pid":     s.PID,
			"command": s.Command,
			"user":    s.User,
			"cpu":     "",
			"mem":     "",
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sessions": out})
}

func handleContainerUpdateAgents(w http.ResponseWriter, r *http.Request, configDir, name string) {
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, err := ensureContainerExecContext(configDir, name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	imgCfg, err := resolveImageConfig(cfg, name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	workdir := imgCfg.Workdir
	if workdir == "" {
		workdir = "/var/www/discourse"
	}

	steps := []agentUpdateStep{
		{label: "OpenAI Codex CLI", command: "npm install -g @openai/codex", runAsRoot: true},
		{label: "Google Gemini CLI", command: "npm install -g @google/gemini-cli", runAsRoot: true},
		{label: "Crush CLI", command: "npm install -g @charmland/crush", runAsRoot: true},
		{label: "Github CLI", command: "npm install -g @github/copilot", runAsRoot: true},
		{label: "OpenCode AI", command: "npm install -g opencode-ai@latest", runAsRoot: true},
		{label: "Amp CLI", command: "npm install -g @sourcegraph/amp", runAsRoot: true},
		{label: "Claude CLI", command: "curl -fsSL https://claude.ai/install.sh | bash", useUserPaths: true},
		{label: "Aider", command: "curl -LsSf https://aider.chat/install.sh | sh", useUserPaths: true},
		{label: "Cursor Agent", command: "curl -fsS https://cursor.com/install | bash", useUserPaths: true},
		{label: "Factory Droid", command: "curl -fsSL https://app.factory.ai/cli | sh", useUserPaths: true},
		{label: "Mistral Vibe", command: "curl -LsSf https://mistral.ai/vibe/install.sh | bash", useUserPaths: true},
		{label: "Term-LLM", command: "command -v term-llm >/dev/null && term-llm upgrade || echo 'term-llm not installed, skipping'", useUserPaths: true},
	}

	streamSequence(w, func(sse *sseWriter) error {
		for _, step := range steps {
			sse.writeEvent("output", map[string]string{"stream": "stdout", "text": fmt.Sprintf("• %s...\n", step.label)})
			shellCmd := "set -euo pipefail; "
			if step.useUserPaths {
				shellCmd += withUserPaths(step.command)
			} else {
				shellCmd += step.command
			}
			argv := []string{"bash", "-lc", shellCmd}
			execFn := func(stdout, stderr io.Writer) error {
				if step.runAsRoot {
					return execStreamAsUser("root", ctx.name, workdir, nil, argv, stdout, stderr)
				}
				return docker.ExecStream(ctx.name, workdir, nil, argv, stdout, stderr)
			}
			if err := runExecWithSSE(sse, execFn); err != nil {
				return err
			}
		}
		return nil
	}, true)
}

func handleContainerLogTail(w http.ResponseWriter, r *http.Request, name, logPath string) {
	lines := 50
	if v := strings.TrimSpace(r.URL.Query().Get("lines")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	argv := []string{"tail", "-n", strconv.Itoa(lines), "-f", logPath}

	streamExec(w, func(stdout, stderr io.Writer) error {
		return execStreamContext(r.Context(), name, "/", nil, argv, stdout, stderr)
	}, false)
}

func handleImages(w http.ResponseWriter, r *http.Request, configDir string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	names := make([]string, 0, len(cfg.Images))
	for name := range cfg.Images {
		names = append(names, name)
	}
	sort.Strings(names)
	var images []map[string]interface{}
	for _, name := range names {
		img := cfg.Images[name]
		images = append(images, map[string]interface{}{
			"name":     name,
			"tag":      img.Tag,
			"kind":     img.Kind,
			"workdir":  img.Workdir,
			"selected": name == cfg.SelectedImage,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"images":   images,
		"selected": cfg.SelectedImage,
	})
}

func handleImageActions(w http.ResponseWriter, r *http.Request, configDir string, parts []string) {
	if len(parts) < 2 {
		writeJSON(w, http.StatusNotFound, "not found")
		return
	}
	action := parts[1]
	switch action {
	case "build":
		handleImageBuild(w, r, configDir)
	case "pull":
		handleImagePull(w, r, configDir)
	default:
		writeJSON(w, http.StatusNotFound, "not found")
	}
}

func handleImageBuild(w http.ResponseWriter, r *http.Request, configDir string) {
	var req struct {
		Target       string   `json:"target"`
		NoCache      bool     `json:"no_cache"`
		BuildArgs    []string `json:"build_args"`
		Tag          string   `json:"tag"`
		ClassicBuild bool     `json:"classic_build"`
		Builder      string   `json:"builder"`
		RmExisting   bool     `json:"rm_existing"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	if req.RmExisting && docker.Exists(cfg.DefaultContainer) {
		_ = docker.Stop(cfg.DefaultContainer)
		_ = docker.Remove(cfg.DefaultContainer)
	}

	target := strings.TrimSpace(req.Target)
	if target == "" {
		target = cfg.SelectedImage
	}

	var dockerfilePath string
	var contextDir string
	var imageTag string

	if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
		dockerfilePath = target
		contextDir = filepath.Dir(target)
		if req.Tag != "" {
			imageTag = req.Tag
		} else if img, ok := cfg.Images[cfg.SelectedImage]; ok {
			imageTag = img.Tag
		}
	} else {
		imgName := target
		img, ok := cfg.Images[imgName]
		if !ok {
			if imgName == "discourse" {
				img = config.ImageConfig{Kind: "discourse", Tag: cfg.ImageTag, Workdir: cfg.Workdir, ContainerPort: cfg.ContainerPort, Dockerfile: config.ImageSource{Source: "stock", StockName: "discourse"}}
			} else {
				writeJSON(w, http.StatusBadRequest, fmt.Sprintf("unknown image '%s'", imgName))
				return
			}
		}

		imageTag = img.Tag
		if req.Tag != "" {
			imageTag = req.Tag
		}

		switch img.Dockerfile.Source {
		case "stock":
			var err error
			dockerfilePath, contextDir, _, err = assets.ResolveDockerfile(configDir)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, err.Error())
				return
			}
		case "path":
			dockerfilePath = img.Dockerfile.Path
			contextDir = filepath.Dir(img.Dockerfile.Path)
		default:
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf("unsupported dockerfile source '%s'", img.Dockerfile.Source))
			return
		}
	}

	if strings.TrimSpace(imageTag) == "" {
		writeJSON(w, http.StatusBadRequest, "image tag required")
		return
	}

	buildArgs := make([]string, 0, len(req.BuildArgs)+2)
	if req.NoCache {
		buildArgs = append(buildArgs, "--no-cache")
	}
	for _, kv := range req.BuildArgs {
		buildArgs = append(buildArgs, "--build-arg", kv)
	}

	cmdName, cmdArgs, cmdEnv := buildDockerBuildCommand(imageTag, dockerfilePath, contextDir, req.ClassicBuild, req.Builder, buildArgs)

	streamHostCommandWithEnv(w, r.Context(), cmdName, cmdArgs, cmdEnv, true)
}

func handleImagePull(w http.ResponseWriter, r *http.Request, configDir string) {
	var req struct {
		ImageName  string `json:"image_name"`
		Tag        string `json:"tag"`
		RmExisting bool   `json:"rm_existing"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	imageName := strings.TrimSpace(req.ImageName)
	if imageName == "" {
		imageName = cfg.SelectedImage
	}
	img, ok := cfg.Images[imageName]
	if !ok {
		writeJSON(w, http.StatusBadRequest, fmt.Sprintf("unknown image '%s'", imageName))
		return
	}

	ref := img.Tag
	if strings.TrimSpace(req.Tag) != "" {
		ref = req.Tag
	} else if img.Kind == "discourse" && img.Tag == "ai_agent" {
		ref = "discourse/dv:latest"
	}
	if ref == "" {
		writeJSON(w, http.StatusBadRequest, "no tag configured")
		return
	}

	streamSequence(w, func(sse *sseWriter) error {
		if req.RmExisting && docker.ImageExists(ref) {
			sse.writeEvent("output", map[string]string{"stream": "stdout", "text": fmt.Sprintf("Removing existing image %s...\n", ref)})
			if err := docker.RemoveImage(ref); err != nil {
				return err
			}
		}
		sse.writeEvent("output", map[string]string{"stream": "stdout", "text": fmt.Sprintf("Pulling Docker image: %s\n", ref)})
		if err := runExecWithSSE(sse, func(stdout, stderr io.Writer) error {
			cmd := exec.CommandContext(r.Context(), "docker", "pull", ref)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			return cmd.Run()
		}); err != nil {
			return err
		}
		if img.Kind == "discourse" && img.Tag != "" && ref == "discourse/dv:latest" && img.Tag != ref {
			sse.writeEvent("output", map[string]string{"stream": "stdout", "text": fmt.Sprintf("Tagging %s as %s\n", ref, img.Tag)})
			cmd := exec.CommandContext(r.Context(), "docker", "tag", ref, img.Tag)
			if err := cmd.Run(); err != nil {
				return err
			}
		}
		return nil
	}, true)
}

func handleConfig(w http.ResponseWriter, r *http.Request, configDir string) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPatch:
		var req struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := setConfigField(&cfg, req.Key, req.Value); err != nil {
			writeJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := config.Save(configDir, cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func listContainers(cfg config.Config, includeSessions bool) ([]map[string]interface{}, string, error) {
	imgName, imgCfg, err := resolveImage(cfg, "")
	if err != nil {
		return nil, "", err
	}
	proxyActive := cfg.LocalProxy.Enabled && localproxy.Running(cfg.LocalProxy)

	out, _ := runShell("docker ps -a --format '{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}\t{{.Labels}}\t{{.CreatedAt}}'")
	selected := strings.TrimSpace(cfg.SelectedAgent)
	if selected == "" {
		selected = cfg.DefaultContainer
	}

	var agents []agentInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) < 3 {
			continue
		}
		name, image, status := parts[0], parts[1], parts[2]
		portsField := ""
		labelsField := ""
		createdAt := time.Time{}
		if len(parts) >= 4 {
			portsField = parts[3]
		}
		if len(parts) >= 5 {
			labelsField = parts[4]
		}
		if len(parts) >= 6 {
			createdAt = parseDockerTime(parts[5])
		}
		labelMap := parseLabels(labelsField)
		belongs := false
		if imgNameFromCfg, ok := cfg.ContainerImages[name]; ok && imgNameFromCfg == imgName {
			belongs = true
		}
		if !belongs {
			if labelMap["com.dv.owner"] == "dv" && labelMap["com.dv.image-name"] == imgName {
				belongs = true
			}
		}
		if !belongs {
			if image == imgCfg.Tag {
				belongs = true
			}
		}
		if !belongs {
			continue
		}
		statusText, timeText := parseStatus(status)
		urls := parseHostPortURLs(portsField)
		if proxyActive {
			if host, _, _, httpPort, ok := localproxy.RouteFromLabels(labelMap); ok && host != "" {
				lp := cfg.LocalProxy
				lp.ApplyDefaults()
				if lp.HTTPS {
					if lp.HTTPSPort > 0 && lp.HTTPSPort != 443 {
						urls = []string{fmt.Sprintf("https://%s:%d", host, lp.HTTPSPort)}
					} else {
						urls = []string{"https://" + host}
					}
				} else {
					if httpPort <= 0 {
						httpPort = lp.HTTPPort
					}
					if httpPort > 0 && httpPort != 80 {
						urls = []string{fmt.Sprintf("http://%s:%d", host, httpPort)}
					} else {
						urls = []string{"http://" + host}
					}
				}
			}
		}

		agents = append(agents, agentInfo{
			name:      name,
			status:    statusText,
			time:      timeText,
			createdAt: createdAt,
			urls:      urls,
			selected:  selected != "" && name == selected,
		})
	}

	sortAgents(agents)
	if includeSessions {
		for i, agent := range agents {
			if agent.status == "Running" {
				s, err := docker.ExecSessions(agent.name)
				if err != nil {
					agents[i].sessions = -1
				} else {
					agents[i].sessions = len(s)
				}
			}
		}
	}

	var outContainers []map[string]interface{}
	for _, agent := range agents {
		data := map[string]interface{}{
			"name":     agent.name,
			"status":   agent.status,
			"time":     agent.time,
			"image":    imgCfg.Tag,
			"urls":     agent.urls,
			"selected": agent.selected,
		}
		if includeSessions {
			data["sessions"] = agent.sessions
		}
		outContainers = append(outContainers, data)
	}

	return outContainers, selected, nil
}

func ensureContainerExecContext(configDir, name string) (containerExecContext, error) {
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return containerExecContext{}, err
	}
	if strings.TrimSpace(name) == "" {
		return containerExecContext{}, fmt.Errorf("container name required")
	}
	if !docker.Exists(name) {
		return containerExecContext{}, fmt.Errorf("container '%s' does not exist", name)
	}
	if !docker.Running(name) {
		if err := docker.Start(name); err != nil {
			return containerExecContext{}, err
		}
	}

	imgName := cfg.ContainerImages[name]
	var imgCfg config.ImageConfig
	if imgName != "" {
		imgCfg = cfg.Images[imgName]
	} else {
		_, imgCfg, err = resolveImage(cfg, "")
		if err != nil {
			return containerExecContext{}, err
		}
	}
	workdir := config.EffectiveWorkdir(cfg, imgCfg, name)
	if strings.TrimSpace(workdir) == "" {
		workdir = "/var/www/discourse"
	}
	envs := collectEnvPassthrough(cfg)

	return containerExecContext{name: name, workdir: workdir, envs: envs}, nil
}

func ensureDiscourseContainer(configDir, name string) (containerExecContext, config.ImageConfig, error) {
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return containerExecContext{}, config.ImageConfig{}, err
	}
	ctx, err := ensureContainerExecContext(configDir, name)
	if err != nil {
		return containerExecContext{}, config.ImageConfig{}, err
	}
	imgName := cfg.ContainerImages[name]
	if imgName == "" {
		imgName = cfg.SelectedImage
	}
	imgCfg, ok := cfg.Images[imgName]
	if !ok {
		return containerExecContext{}, config.ImageConfig{}, fmt.Errorf("unknown image '%s'", imgName)
	}
	if imgCfg.Kind != "discourse" {
		return containerExecContext{}, config.ImageConfig{}, fmt.Errorf("image '%s' is not discourse kind", imgName)
	}
	return ctx, imgCfg, nil
}

func buildDockerBuildCommand(tag, dockerfilePath, contextDir string, classic bool, builder string, extraArgs []string) (string, []string, []string) {
	useBuildx := false
	if !classic {
		if err := exec.Command("docker", "buildx", "version").Run(); err == nil {
			useBuildx = true
		}
	}
	if useBuildx {
		args := []string{"buildx", "build", "--load", "-t", tag, "-f", dockerfilePath}
		if strings.TrimSpace(builder) != "" {
			args = append(args, "--builder", strings.TrimSpace(builder))
		}
		args = append(args, extraArgs...)
		args = append(args, contextDir)
		return "docker", args, []string{"DOCKER_BUILDKIT=1"}
	}
	args := []string{"build", "-t", tag, "-f", dockerfilePath}
	args = append(args, extraArgs...)
	args = append(args, contextDir)
	return "docker", args, []string{"DOCKER_BUILDKIT=1"}
}

func streamExec(w http.ResponseWriter, execFn func(stdout, stderr io.Writer) error, sendDone bool) {
	sse, stop, err := startSSE(w)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	err = runExecWithSSE(sse, execFn)
	stop()
	if sendDone {
		sse.writeEvent("done", map[string]interface{}{"exit_code": exitCode(err)})
	}
}

func streamSequence(w http.ResponseWriter, run func(*sseWriter) error, sendDone bool) {
	sse, stop, err := startSSE(w)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	err = run(sse)
	stop()
	if sendDone {
		sse.writeEvent("done", map[string]interface{}{"exit_code": exitCode(err)})
	}
}

func startSSE(w http.ResponseWriter) (*sseWriter, func(), error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, nil, fmt.Errorf("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sse := &sseWriter{w: w, flusher: flusher}
	stopCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sse.writeComment("keep-alive")
			case <-stopCh:
				return
			}
		}
	}()
	return sse, func() { close(stopCh) }, nil
}

func runExecWithSSE(sse *sseWriter, execFn func(stdout, stderr io.Writer) error) error {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(2)
	go scanStream(stdoutR, "stdout", sse, &wg)
	go scanStream(stderrR, "stderr", sse, &wg)

	err := execFn(stdoutW, stderrW)
	_ = stdoutW.Close()
	_ = stderrW.Close()
	wg.Wait()
	return err
}

func scanStream(r io.Reader, stream string, sse *sseWriter, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		sse.writeEvent("output", map[string]string{
			"stream": stream,
			"text":   scanner.Text() + "\n",
		})
	}
}

func streamHostCommand(w http.ResponseWriter, ctx context.Context, name string, args []string, sendDone bool) {
	streamHostCommandWithEnv(w, ctx, name, args, nil, sendDone)
}

func streamHostCommandWithEnv(w http.ResponseWriter, ctx context.Context, name string, args []string, env []string, sendDone bool) {
	streamExec(w, func(stdout, stderr io.Writer) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if len(env) > 0 {
			cmd.Env = append(os.Environ(), env...)
		}
		return cmd.Run()
	}, sendDone)
}

func execStreamAsUser(user, name, workdir string, envs docker.Envs, argv []string, stdout, stderr io.Writer) error {
	args := []string{"exec", "--user", user, "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.Command("docker", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func execStreamContext(ctx context.Context, name, workdir string, envs docker.Envs, argv []string, stdout, stderr io.Writer) error {
	args := []string{"exec", "--user", "discourse", "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	body := map[string]interface{}{}
	if status >= 400 {
		msg := payload
		if err, ok := payload.(error); ok {
			msg = err.Error()
		}
		body["ok"] = false
		body["error"] = msg
	} else {
		body["ok"] = true
		body["data"] = payload
	}

	_ = json.NewEncoder(w).Encode(body)
}

func writeSSE(w io.Writer, event string, data interface{}) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\n", event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	return err
}

func decodeJSON(r *http.Request, dst interface{}) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
