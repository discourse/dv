package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	list "charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"dv/internal/config"
	"dv/internal/docker"
)

type fakeTUIBackend struct {
	mu         sync.Mutex
	inventory  tuiInventory
	loadErr    error
	sessions   []docker.ExecSession
	sessionErr error
	output     string
	runErr     error
	runs       []tuiActionRequest
}

func (f *fakeTUIBackend) Load(context.Context) (tuiInventory, error) {
	return f.inventory, f.loadErr
}

func (f *fakeTUIBackend) Sessions(context.Context, string) ([]docker.ExecSession, error) {
	return f.sessions, f.sessionErr
}

func (f *fakeTUIBackend) Run(_ context.Context, request tuiActionRequest, output io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, request)
	if f.output != "" {
		_, _ = io.WriteString(output, f.output)
	}
	return f.runErr
}

func (f *fakeTUIBackend) requests() []tuiActionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tuiActionRequest(nil), f.runs...)
}

func testTUIInventory() tuiInventory {
	cfg := config.Default()
	cfg.SelectedAgent = "beta"
	return tuiInventory{
		cfg:           cfg,
		imageName:     "discourse",
		image:         cfg.Images["discourse"],
		selectedAgent: "beta",
		agents: []agentItem{
			{name: "alpha", imageName: "discourse", imageTag: "ai_agent", status: "Stopped"},
			{name: "beta", imageName: "discourse", imageTag: "ai_agent", status: "Running", selected: true},
		},
	}
}

func newTestTUI(t *testing.T) (model, *fakeTUIBackend) {
	t.Helper()
	backend := &fakeTUIBackend{inventory: testTUIInventory()}
	m := newTUIModel(context.Background(), backend)
	m.resize(100, 30)
	m.applyInventory(backend.inventory)
	m.loading = false
	return m, backend
}

func printableKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func specialKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

func updateTUI(t *testing.T, m model, msg tea.Msg) (model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	updated, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want cli.model", next)
	}
	return updated, cmd
}

func commandMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	messages := make([]tea.Msg, 0, len(batch))
	for _, child := range batch {
		messages = append(messages, commandMessages(child)...)
	}
	return messages
}

func TestTUIItemTitleHasNoEmbeddedLeadingPadding(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	item := agentItem{name: "shared-edits"}
	if got := item.Title(); got != "shared-edits" {
		t.Fatalf("item title = %q, want unpadded name", got)
	}
	running := agentItem{name: "shared-edits", status: "Running"}
	if got := running.Title(); got != "shared-edits  ●" {
		t.Fatalf("running item title = %q, want trailing running marker", got)
	}
	selected := agentItem{name: "patch", selected: true}
	if got := selected.Title(); got != "patch  ✓" {
		t.Fatalf("selected item title = %q, want trailing selection marker", got)
	}
}

func TestTUIInitialLoadIsAsynchronous(t *testing.T) {
	backend := &fakeTUIBackend{inventory: testTUIInventory()}
	m := newTUIModel(context.Background(), backend)
	if !m.loading {
		t.Fatal("new model should be loading")
	}

	messages := commandMessages(m.loadCmd())
	if len(messages) != 1 {
		t.Fatalf("load command returned %d messages, want 1", len(messages))
	}
	m, _ = updateTUI(t, m, messages[0])
	if m.loading || len(m.inventory.agents) != 2 {
		t.Fatalf("inventory was not applied: loading=%v agents=%d", m.loading, len(m.inventory.agents))
	}
}

func TestTUICommittedFilterBlocksRefreshAndPreservesCursor(t *testing.T) {
	m, _ := newTestTUI(t)
	m, _ = updateTUI(t, m, printableKey('/'))
	for _, r := range "beta" {
		m, _ = updateTUI(t, m, printableKey(r))
	}
	m, _ = updateTUI(t, m, specialKey(tea.KeyEnter))
	if m.list.FilterState() == list.Unfiltered {
		t.Fatal("filter was not committed")
	}
	beforeID := m.loadID
	m, _ = updateTUI(t, m, refreshTickMsg{})
	if m.loadID != beforeID {
		t.Fatal("background refresh ran while a filter was applied")
	}
	m.applyInventory(testTUIInventory())
	if got := m.selectedAgent(); got != "beta" {
		t.Fatalf("filtered cursor moved to %q", got)
	}
}

func TestTUIFilteringOwnsPrintableShortcutKeys(t *testing.T) {
	m, backend := newTestTUI(t)
	m, _ = updateTUI(t, m, printableKey('/'))
	if m.list.FilterState() != list.Filtering {
		t.Fatalf("filter state = %v, want Filtering", m.list.FilterState())
	}

	for _, r := range []rune{'n', 's', 'S', 'd', 'e', 'x', 'b', 'i', 'l', 'r', 'q'} {
		m, _ = updateTUI(t, m, printableKey(r))
		if m.busy || m.pendingEnter != "" || m.modal != modalNone {
			t.Fatalf("filter key %q triggered UI action: busy=%v enter=%q modal=%v", r, m.busy, m.pendingEnter, m.modal)
		}
	}
	if got := len(backend.requests()); got != 0 {
		t.Fatalf("filtering triggered %d backend requests", got)
	}
}

func TestTUIJKNavigationDoesNotStopContainer(t *testing.T) {
	m, _ := newTestTUI(t)
	m.list.Select(1)
	m, _ = updateTUI(t, m, printableKey('k'))
	if got := m.selectedAgent(); got != "alpha" {
		t.Fatalf("k selected %q, want alpha", got)
	}
	if m.busy {
		t.Fatal("k unexpectedly started an operation")
	}
}

func TestTUIRemoveStoppedContainerSkipsSessionProbe(t *testing.T) {
	m, backend := newTestTUI(t)
	backend.sessionErr = errors.New("docker top cannot inspect a stopped container")
	m.list.Select(0) // alpha is stopped

	m, cmd := updateTUI(t, m, printableKey('d'))
	if cmd != nil || m.busy {
		t.Fatalf("stopped remove started session probe: busy=%v cmd=%v", m.busy, cmd)
	}
	if m.modal != modalConfirm || m.confirm.target != "alpha" {
		t.Fatalf("stopped remove did not open confirmation: modal=%v target=%q", m.modal, m.confirm.target)
	}
}

func TestTUIRemoveRequiresConfirmationAndShowsSessions(t *testing.T) {
	m, backend := newTestTUI(t)
	backend.sessions = []docker.ExecSession{{PID: 42, Command: "claude --dangerously-skip-permissions"}}

	m, cmd := updateTUI(t, m, printableKey('d'))
	if !m.busy || m.operation != "checking active sessions" {
		t.Fatalf("remove did not check sessions: busy=%v operation=%q", m.busy, m.operation)
	}
	for _, msg := range commandMessages(cmd) {
		if _, ok := msg.(sessionCheckMsg); ok {
			m, _ = updateTUI(t, m, msg)
		}
	}
	if m.modal != modalConfirm || len(m.sessions) != 1 {
		t.Fatalf("confirmation not shown: modal=%v sessions=%d", m.modal, len(m.sessions))
	}
	if len(backend.requests()) != 0 {
		t.Fatal("remove ran before confirmation")
	}

	m, cmd = updateTUI(t, m, printableKey('y'))
	for _, msg := range commandMessages(cmd) {
		if _, ok := msg.(operationDoneMsg); ok {
			m, _ = updateTUI(t, m, msg)
		}
	}
	requests := backend.requests()
	if len(requests) != 1 || requests[0].action != actionRemove || !requests[0].force {
		t.Fatalf("remove request = %#v", requests)
	}
}

func TestTUIStopWithoutSessionsRunsWithoutPrompt(t *testing.T) {
	m, backend := newTestTUI(t)
	m, cmd := updateTUI(t, m, printableKey('S'))
	for _, msg := range commandMessages(cmd) {
		if _, ok := msg.(sessionCheckMsg); ok {
			m, cmd = updateTUI(t, m, msg)
		}
	}
	if m.modal != modalNone || !m.busy {
		t.Fatalf("stop state: modal=%v busy=%v", m.modal, m.busy)
	}
	for _, msg := range commandMessages(cmd) {
		if _, ok := msg.(operationDoneMsg); ok {
			m, _ = updateTUI(t, m, msg)
		}
	}
	requests := backend.requests()
	if len(requests) != 1 || requests[0].action != actionStop || !requests[0].force {
		t.Fatalf("stop request = %#v", requests)
	}
}

func TestTUIStartRunsInBackgroundWithoutSuspendingAltScreen(t *testing.T) {
	m, backend := newTestTUI(t)
	m, cmd := updateTUI(t, m, printableKey('s'))
	if !m.busy || cmd == nil {
		t.Fatalf("start did not begin a background operation: busy=%v cmd=%v", m.busy, cmd)
	}
	messages := commandMessages(cmd)
	requests := backend.requests()
	if len(requests) != 1 || requests[0].action != actionStart || requests[0].target != "beta" {
		t.Fatalf("start requests = %#v", requests)
	}
	for _, msg := range messages {
		if _, ok := msg.(operationDoneMsg); ok {
			m, _ = updateTUI(t, m, msg)
		}
	}
	if m.busy {
		t.Fatal("start remained busy after completion")
	}
}

func TestTUIInteractiveActionsDoNotReenterBackgroundBackend(t *testing.T) {
	m, backend := newTestTUI(t)
	m, cmd := updateTUI(t, m, printableKey('n'))
	if !m.busy || m.operation != string(actionNew) || cmd == nil {
		t.Fatalf("new action state: busy=%v operation=%q cmd=%v", m.busy, m.operation, cmd)
	}
	if len(backend.requests()) != 0 {
		t.Fatal("interactive new action was sent to background backend")
	}

	id := m.operationID
	m, _ = updateTUI(t, m, interactiveDoneMsg{
		id:      id,
		request: tuiActionRequest{action: actionNew, target: "new-agent"},
		output:  "Created new-agent\n",
	})
	if m.busy || !strings.Contains(m.logText, "Created new-agent") {
		t.Fatalf("interactive completion state: busy=%v log=%q", m.busy, m.logText)
	}
}

func TestTUIStreamsOperationProgressBeforeCompletion(t *testing.T) {
	m, backend := newTestTUI(t)
	backend.output = "step one\n"
	m, cmd := updateTUI(t, m, printableKey('b'))
	var wait tea.Cmd
	for _, msg := range commandMessages(cmd) {
		if _, ok := msg.(operationProgressMsg); ok {
			m, wait = updateTUI(t, m, msg)
		}
	}
	if !strings.Contains(m.logText, "step one") || !m.busy {
		t.Fatalf("progress state: log=%q busy=%v", m.logText, m.busy)
	}
	for _, msg := range commandMessages(wait) {
		if _, ok := msg.(operationDoneMsg); ok {
			m, _ = updateTUI(t, m, msg)
		}
	}
	if m.busy {
		t.Fatal("operation remained busy after completion")
	}
}

func TestTUIOperationResultUpdatesLiveLogAndError(t *testing.T) {
	m, _ := newTestTUI(t)
	m.busy = true
	m.operationID = 7
	failure := errors.New("docker exploded")
	m, _ = updateTUI(t, m, operationProgressMsg{id: 7, text: "starting beta\n"})
	msg := operationDoneMsg{
		id:      7,
		request: tuiActionRequest{action: actionStart, target: "beta"},
		err:     failure,
	}
	m, _ = updateTUI(t, m, msg)
	for _, want := range []string{"starting beta", "docker exploded"} {
		if !strings.Contains(m.logText, want) {
			t.Errorf("log %q does not contain %q", m.logText, want)
		}
	}
	if m.busy || m.errText != failure.Error() {
		t.Fatalf("completion state: busy=%v error=%q", m.busy, m.errText)
	}
}

func TestTUISessionProbeFailureCanBeForcedWithConfirmation(t *testing.T) {
	m, _ := newTestTUI(t)
	m.busy = true
	m.operationID = 21
	request := tuiActionRequest{action: actionRemove, target: "beta"}
	m, _ = updateTUI(t, m, sessionCheckMsg{id: 21, request: request, err: errors.New("docker top failed")})
	if m.modal != modalConfirm || !strings.Contains(m.sessionWarning, "docker top failed") {
		t.Fatalf("probe failure did not open warning confirmation: modal=%v warning=%q", m.modal, m.sessionWarning)
	}
	m, _ = updateTUI(t, m, printableKey('y'))
	if !m.busy || m.currentAction != actionRemove {
		t.Fatalf("forced confirmation did not start remove: busy=%v action=%q", m.busy, m.currentAction)
	}
}

func TestTUILogChunksPreserveOriginalLineBoundaries(t *testing.T) {
	m, _ := newTestTUI(t)
	m.appendLog("partial")
	m.appendLog(" line")
	m.appendLog("\n")
	if got := m.logText; got != "partial line\n" {
		t.Fatalf("stream chunks were rewritten: %q", got)
	}
}

func TestTUICtrlCWaitsForDestructiveOperation(t *testing.T) {
	m, _ := newTestTUI(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.busy = true
	m.currentAction = actionRename
	m.operation = string(actionRename)
	m.cancel = cancel

	m, _ = updateTUI(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !m.busy || !m.quitAfterOp {
		t.Fatalf("Ctrl+C did not defer quit: busy=%v quitAfter=%v", m.busy, m.quitAfterOp)
	}
	select {
	case <-ctx.Done():
		t.Fatal("Ctrl+C canceled an atomic rename")
	default:
	}
}

func TestTUIDestructiveMutationCannotBeCanceledMidFlight(t *testing.T) {
	m, _ := newTestTUI(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.busy = true
	m.operation = string(actionRemove)
	m.currentAction = actionRemove
	m.cancel = cancel

	m, _ = updateTUI(t, m, specialKey(tea.KeyEscape))
	if !m.busy || m.operation != string(actionRemove) {
		t.Fatalf("remove was canceled mid-flight: busy=%v operation=%q", m.busy, m.operation)
	}
	select {
	case <-ctx.Done():
		t.Fatal("remove context was canceled mid-flight")
	default:
	}
}

func TestTUICancelDuringSessionCheckInvalidatesResult(t *testing.T) {
	m, backend := newTestTUI(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.busy = true
	m.operation = "checking active sessions"
	m.operationID = 11
	m.cancel = cancel

	m, _ = updateTUI(t, m, specialKey(tea.KeyEscape))
	if m.busy || m.operationID == 11 {
		t.Fatalf("cancel did not release/invalidate operation: busy=%v id=%d", m.busy, m.operationID)
	}
	m, _ = updateTUI(t, m, sessionCheckMsg{
		id:      11,
		request: tuiActionRequest{action: actionStop, target: "beta"},
	})
	if m.modal != modalNone || len(backend.requests()) != 0 {
		t.Fatalf("stale session result continued action: modal=%v requests=%#v", m.modal, backend.requests())
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("session context was not canceled")
	}
}

func TestTUIIgnoresOutOfOrderInventoryLoads(t *testing.T) {
	m, _ := newTestTUI(t)
	m.loadID = 9
	stale := testTUIInventory()
	stale.imageName = "stale"
	m, _ = updateTUI(t, m, inventoryMsg{id: 8, inventory: stale})
	if m.inventory.imageName != "discourse" {
		t.Fatalf("stale inventory was applied: %q", m.inventory.imageName)
	}
}

func TestTUILogBufferIsBounded(t *testing.T) {
	m, _ := newTestTUI(t)
	m.appendLog(strings.Repeat("x", maxTUILogBytes+4096))
	if len(m.logText) > maxTUILogBytes+len("… older output discarded …\n") {
		t.Fatalf("log buffer grew beyond cap: %d", len(m.logText))
	}
	if !strings.HasPrefix(m.logText, "… older output discarded …") {
		t.Fatalf("bounded log omitted truncation marker: %q", m.logText[:minInt(80, len(m.logText))])
	}
}

func TestTUICancelKeyCancelsBusyOperation(t *testing.T) {
	m, _ := newTestTUI(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.busy = true
	m.cancel = cancel
	m, _ = updateTUI(t, m, specialKey(tea.KeyEscape))
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Esc did not cancel the active operation")
	}
}

func TestTUIDenseDelegateUsesResponsiveSingleLineRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m, _ := newTestTUI(t)
	item := agentItem{
		name: "shared-edits", status: "Running", age: "48 minutes",
		urls: []string{"https://shared-edits.dev.home.arpa"}, selected: true,
	}
	delegate := agentDelegate{}
	if delegate.Height() != 1 || delegate.Spacing() != 0 {
		t.Fatalf("delegate density = height %d spacing %d", delegate.Height(), delegate.Spacing())
	}

	m.list.SetSize(120, 20)
	var wide strings.Builder
	delegate.Render(&wide, m.list, 0, item)
	wideRow := ansi.Strip(wide.String())
	if strings.Contains(wideRow, "\n") || !strings.Contains(wideRow, "shared-edits  ●  ✓") || !strings.Contains(wideRow, item.urls[0]) {
		t.Fatalf("wide row is not dense or complete: %q", wideRow)
	}

	m.list.SetSize(48, 20)
	var narrow strings.Builder
	delegate.Render(&narrow, m.list, 0, item)
	narrowRow := ansi.Strip(narrow.String())
	if strings.Contains(narrowRow, item.urls[0]) {
		t.Fatalf("narrow row retained low-priority URL: %q", narrowRow)
	}
	if lipgloss.Width(narrow.String()) > 48 {
		t.Fatalf("narrow row overflowed: width=%d", lipgloss.Width(narrow.String()))
	}
}

func TestTUIModalOverlayPreservesContext(t *testing.T) {
	m, _ := newTestTUI(t)
	m.modal = modalConfirm
	m.confirm = tuiActionRequest{action: actionRemove, target: "beta"}
	view := ansi.Strip(m.viewString())
	if !strings.Contains(view, "image: discourse") {
		t.Fatalf("modal removed background context: %q", view)
	}
	if !strings.Contains(view, "Remove beta?") {
		t.Fatalf("confirmation modal missing from overlay: %q", view)
	}
}

func TestTUIResponsiveChromeTiers(t *testing.T) {
	m, _ := newTestTUI(t)
	for _, width := range []int{60, 80, 120} {
		header := m.renderStatus(width)
		footer := m.renderFooter(width)
		if got := lipgloss.Width(header); got != width {
			t.Errorf("header width at %d columns = %d", width, got)
		}
		if got := lipgloss.Width(footer); got > width {
			t.Errorf("footer width at %d columns = %d", width, got)
		}
	}
	if got := ansi.Strip(m.renderStatus(60)); strings.Contains(got, "agents") {
		t.Fatalf("narrow header retained agent count: %q", got)
	}
	if got := ansi.Strip(m.renderStatus(120)); !strings.Contains(got, "2 agents") {
		t.Fatalf("wide header omitted agent count: %q", got)
	}

	m.resize(10, 5)
	for _, line := range strings.Split(m.viewString(), "\n") {
		if width := lipgloss.Width(line); width > 10 {
			t.Fatalf("tiny layout overflowed: line width=%d", width)
		}
	}

	m.resize(60, 12)
	m.errText = "daemon unavailable"
	view := ansi.Strip(m.viewString())
	if !strings.Contains(view, "Error: daemon unavailable") || strings.Contains(view, "n new") {
		t.Fatalf("short layout did not replace footer with error: %q", view)
	}
}

func TestTUIIdleSpinnerStopsAndBackgroundRefreshIsSilent(t *testing.T) {
	m, _ := newTestTUI(t)
	m.loading = false
	m.busy = false

	tick := m.spinner.Tick()
	m, cmd := updateTUI(t, m, tick)
	if cmd != nil {
		t.Fatal("idle spinner scheduled another frame")
	}

	m, cmd = updateTUI(t, m, refreshTickMsg{})
	if m.loading {
		t.Fatal("background refresh should not show loading state")
	}
	if cmd == nil {
		t.Fatal("background refresh did not schedule the next refresh/load")
	}
}

func TestTUIDetailsUseSimpleDividerWithoutFocusBadge(t *testing.T) {
	m, _ := newTestTUI(t)
	detail := m.renderDetail(50, 20)
	if !strings.Contains(detail, "│") {
		t.Fatalf("details have no section divider: %q", detail)
	}
	for _, distracting := range []string{"╭", "╮", "╰", "╯", "[focused]"} {
		if strings.Contains(detail, distracting) {
			t.Fatalf("details contain distracting decoration %q: %q", distracting, detail)
		}
	}
}

func TestTUIHelpClosesWithoutQuitting(t *testing.T) {
	m, _ := newTestTUI(t)
	m, _ = updateTUI(t, m, printableKey('?'))
	if m.modal != modalHelp {
		t.Fatalf("modal = %v, want help", m.modal)
	}
	m, cmd := updateTUI(t, m, printableKey('q'))
	if m.modal != modalNone || cmd != nil {
		t.Fatalf("q should close help without quitting: modal=%v cmd=%v", m.modal, cmd)
	}
}

func TestTUILayoutClampsSmallTerminalAndLogsAreOptIn(t *testing.T) {
	m, _ := newTestTUI(t)
	m, _ = updateTUI(t, m, tea.WindowSizeMsg{Width: 40, Height: 15})
	if m.list.Width() < 20 || m.list.Height() < 5 {
		t.Fatalf("small layout dimensions: list=%dx%d", m.list.Width(), m.list.Height())
	}
	if m.logsVisible() || m.showLogs {
		t.Fatal("logs should be hidden by default")
	}
	if got := m.viewString(); strings.Contains(got, "Compact view") {
		t.Fatalf("hidden logs should not produce a compact-view warning: %q", got)
	}

	m, _ = updateTUI(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.logsVisible() {
		t.Fatal("logs should remain hidden in a large layout until toggled")
	}
	m, _ = updateTUI(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	if m.list.Width() < 139 {
		t.Fatalf("wide layout wastes space on details: list width=%d", m.list.Width())
	}
	for _, line := range strings.Split(m.viewString(), "\n") {
		if width := lipgloss.Width(line); width > 200 {
			t.Fatalf("responsive layout overflowed terminal: line width=%d", width)
		}
	}
	m, _ = updateTUI(t, m, printableKey('l'))
	if !m.logsVisible() {
		t.Fatal("l should show logs in a large layout")
	}
	m, _ = updateTUI(t, m, specialKey(tea.KeyTab))
	if m.focus != focusLogs {
		t.Fatalf("focus = %v, want logs", m.focus)
	}
	m, _ = updateTUI(t, m, printableKey('d'))
	if m.busy || m.modal != modalNone {
		t.Fatal("agent action fired while logs were focused")
	}
	m, _ = updateTUI(t, m, printableKey('l'))
	if m.logsVisible() || m.focus != focusAgents {
		t.Fatalf("hiding logs should return focus to agents: visible=%v focus=%v", m.logsVisible(), m.focus)
	}
}

func TestTUIHeaderIsConcise(t *testing.T) {
	m, _ := newTestTUI(t)
	header := m.renderStatus(120)
	if strings.Contains(m.viewString(), "dv agents") {
		t.Fatalf("view contains redundant title: %q", m.viewString())
	}
	if strings.Contains(header, "updated") {
		t.Fatalf("header includes distracting update timestamp: %q", header)
	}
	if !strings.Contains(header, "image: discourse") || !strings.Contains(header, "selected: beta") {
		t.Fatalf("header lost useful context: %q", header)
	}
}

func TestTUIEmptyAndDockerErrorStatesAreActionable(t *testing.T) {
	m, _ := newTestTUI(t)
	m.inventory.agents = nil
	m.list.SetItems(nil)
	m.loading = false
	m.resize(120, 30)
	if got := m.viewString(); !strings.Contains(got, "Press n to create one") {
		t.Fatalf("empty state is not actionable: %q", got)
	}
	for _, line := range strings.Split(m.viewString(), "\n") {
		if width := lipgloss.Width(line); width > 120 {
			t.Fatalf("wide empty state overflowed: line width=%d", width)
		}
	}

	m.lastError = errors.New("Cannot connect to Docker")
	if got := m.viewString(); !strings.Contains(got, "Check that Docker is running") {
		t.Fatalf("docker error state is not actionable: %q", got)
	}
}
