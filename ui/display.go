package ui

import (
	"bytes"
	"container/ring"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/morikuni/aec"
	digest "github.com/opencontainers/go-digest"
	"github.com/tonistiigi/units"
	"github.com/vito/progrock/graph"
	"github.com/vito/vt100"
)

type Reader interface {
	ReadStatus() (*graph.SolveStatus, bool)
}

func DisplaySolveStatus(interrupt context.CancelFunc, w io.Writer, r Reader, tui bool) error {
	return Default.DisplaySolveStatus(interrupt, w, r, tui)
}

func (ui Components) DisplaySolveStatus(interrupt context.CancelFunc, w io.Writer, r Reader, tui bool) error {
	model := NewModel(interrupt, w, ui, tui)

	opts := []tea.ProgramOption{tea.WithOutput(w)}

	if tui {
		opts = append(opts, tea.WithAltScreen(), tea.WithMouseCellMotion())
	} else {
		opts = append(opts, tea.WithInput(nil), tea.WithoutRenderer())
	}

	prog := tea.NewProgram(model, opts...)

	go func() {
		for {
			status, ok := r.ReadStatus()
			if ok {
				prog.Send(statusMsg(status))
			} else {
				prog.Send(eofMsg{})
				break
			}
		}
	}()

	if err := prog.Start(); err != nil {
		return err
	}

	model.Print(w)

	return nil
}

func NewModel(interrupt context.CancelFunc, w io.Writer, ui Components, tui bool) *Model {
	return &Model{
		t: newTrace(ui, tui),

		tui:     tui,
		disp:    &display{ui: ui},
		printer: &textMux{w: w, ui: ui},

		interrupt: interrupt,
	}
}

type Model struct {
	t *trace

	tui     bool
	disp    *display
	printer *textMux

	interrupt func()

	hasViewport bool
	viewport    viewport.Model
}

const headerHeight = 0
const footerHeight = 1
const chromeHeight = headerHeight + footerHeight

func (m *Model) SetWindowSize(w, h int) {
	if m.hasViewport {
		m.viewport.Width = w
		m.viewport.Height = h - chromeHeight
	} else {
		m.viewport = viewport.Model{
			Width:  w,
			Height: h - chromeHeight,
		}

		m.hasViewport = true
	}
}

func (m *Model) StatusUpdate(status *graph.SolveStatus) {
	m.t.update(status, m.vtermHeight(), m.viewportWidth())
}

func (model *Model) Print(w io.Writer) {
	if model.tui {
		model.disp.print(
			w,
			model.t.displayInfo(),
			model.vtermHeight(),
			model.viewportWidth(),
			model.viewportHeight(),
		)

		model.t.printErrorLogs(w)
	} else {
		model.printer.print(model.t)
	}
}

type statusMsg *graph.SolveStatus

type eofMsg struct{}

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (tui *Model) Init() tea.Cmd {
	return tick()
}

func (m *Model) viewportWidth() int {
	width := m.viewport.Width
	if width == 0 {
		width = 80
	}

	return width
}

func (m *Model) viewportHeight() int {
	height := m.viewport.Height
	if height == 0 {
		height = 24
	}

	return height
}

func (m *Model) vtermHeight() int {
	return m.viewportHeight() / 3
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		cmd  tea.Cmd
		cmds []tea.Cmd
	)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if k := msg.String(); k == "ctrl+c" || k == "q" || k == "esc" {
			m.interrupt()
			return m, nil
		}

	case tea.WindowSizeMsg:
		if !m.tui {
			break
		}

		m.SetWindowSize(msg.Width, msg.Height)

	case statusMsg:
		m.StatusUpdate(msg)

	case eofMsg:
		return m, tea.Quit

	case tickMsg:
		if m.tui && m.hasViewport {
			buf := new(bytes.Buffer)
			m.disp.print(buf, m.t.displayInfo(), m.vtermHeight(), m.viewportWidth(), m.viewportHeight())

			atBottom := m.viewport.AtBottom()
			m.viewport.SetContent(buf.String())
			if atBottom {
				m.viewport.GotoBottom()
			}
		} else {
			m.printer.print(m.t)
		}

		cmds = append(cmds, tick())
	}

	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

func (m *Model) View() string {
	status := m.disp.status(m.t.displayInfo())
	return fmt.Sprintf("%s\n%s", status, m.viewport.View())
}

const termPad = 4

type displayInfo struct {
	startTime      time.Time
	jobs           []*job
	countTotal     int
	countCompleted int
}

type job struct {
	startTime     *time.Time
	completedTime *time.Time
	name          string
	status        string
	statuses      []*job
	hasError      bool
	isCanceled    bool
	vertex        *vertex
}

type trace struct {
	ui            Components
	localTimeDiff time.Duration
	vertexes      []*vertex
	byDigest      map[digest.Digest]*vertex
	nextIndex     int
	updates       map[digest.Digest]struct{}
	tui           bool
}

type vertex struct {
	*graph.Vertex
	statuses []*status
	byID     map[string]*status
	indent   string
	index    int

	logs          [][]byte
	logsPartial   bool
	logsOffset    int
	logsBuffer    *ring.Ring // stores last logs to print them on error
	prev          *graph.Vertex
	lastBlockTime *time.Time
	count         int
	statusUpdates map[string]struct{}

	jobs      []*job
	jobCached bool

	term      *vt100.VT100
	termBytes int
}

func (v *vertex) update(c int) {
	if v.count == 0 {
		now := time.Now()
		v.lastBlockTime = &now
	}
	v.count += c
}

type status struct {
	*graph.VertexStatus
}

func newTrace(ui Components, tui bool) *trace {
	return &trace{
		byDigest: make(map[digest.Digest]*vertex),
		updates:  make(map[digest.Digest]struct{}),
		tui:      tui,
		ui:       ui,
	}
}

func (t *trace) triggerVertexEvent(v *graph.Vertex) {
	if v.Started == nil {
		return
	}

	var old graph.Vertex
	vtx := t.byDigest[v.Digest]
	if v := vtx.prev; v != nil {
		old = *v
	}

	changed := false
	if v.Digest != old.Digest {
		changed = true
	}
	if v.Name != old.Name {
		changed = true
	}
	if v.Started != old.Started {
		if v.Started != nil && old.Started == nil || !v.Started.Equal(*old.Started) {
			changed = true
		}
	}
	if v.Completed != old.Completed && v.Completed != nil {
		changed = true
	}
	if v.Cached != old.Cached {
		changed = true
	}
	if v.Error != old.Error {
		changed = true
	}

	if changed {
		vtx.update(1)
		t.updates[v.Digest] = struct{}{}
	}

	t.byDigest[v.Digest].prev = v
}

func (t *trace) update(s *graph.SolveStatus, termHeight, termWidth int) {
	for _, v := range s.Vertexes {
		prev, ok := t.byDigest[v.Digest]
		if !ok {
			t.nextIndex++
			t.byDigest[v.Digest] = &vertex{
				byID:          make(map[string]*status),
				statusUpdates: make(map[string]struct{}),
				index:         t.nextIndex,
			}
			if t.tui {
				t.byDigest[v.Digest].term = vt100.NewVT100(termHeight, termWidth-termPad)
			}
		}
		t.triggerVertexEvent(v)
		if v.Started != nil && (prev == nil || prev.Started == nil) {
			if t.localTimeDiff == 0 {
				t.localTimeDiff = time.Since(*v.Started)
			}
			t.vertexes = append(t.vertexes, t.byDigest[v.Digest])
		}
		// allow a duplicate initial vertex that shouldn't reset state
		if !(prev != nil && prev.Started != nil && v.Started == nil) {
			t.byDigest[v.Digest].Vertex = v
		}
		t.byDigest[v.Digest].jobCached = false
	}
	for _, s := range s.Statuses {
		v, ok := t.byDigest[s.Vertex]
		if !ok {
			continue // shouldn't happen
		}
		v.jobCached = false
		prev, ok := v.byID[s.ID]
		if !ok {
			v.byID[s.ID] = &status{VertexStatus: s}
		}
		if s.Started != nil && (prev == nil || prev.Started == nil) {
			v.statuses = append(v.statuses, v.byID[s.ID])
		}
		v.byID[s.ID].VertexStatus = s
		v.statusUpdates[s.ID] = struct{}{}
		t.updates[v.Digest] = struct{}{}
		v.update(1)
	}
	for _, l := range s.Logs {
		v, ok := t.byDigest[l.Vertex]
		if !ok {
			continue // shouldn't happen
		}
		v.jobCached = false
		if v.term != nil {
			if v.term.Width != termWidth {
				v.term.Resize(termHeight, termWidth-termPad)
			}
			v.termBytes += len(l.Data)
			v.term.Write(l.Data) // error unhandled on purpose. don't trust vt100
		}
		i := 0
		complete := split(l.Data, '\n', func(dt []byte) {
			if v.logsPartial && len(v.logs) != 0 && i == 0 {
				v.logs[len(v.logs)-1] = append(v.logs[len(v.logs)-1], dt...)
			} else {
				ts := time.Duration(0)
				if v.Started != nil {
					ts = l.Timestamp.Sub(*v.Started)
				}

				v.logs = append(v.logs, []byte(fmt.Sprintf(t.ui.TextLogFormat, v.index, duration(t.ui, ts, v.Completed != nil), dt)))
			}
			i++
		})
		v.logsPartial = !complete
		t.updates[v.Digest] = struct{}{}
		v.update(1)
	}

	// chronological order based on last activity
	sort.Slice(t.vertexes, func(i, j int) bool {
		iv := t.vertexes[i]
		jv := t.vertexes[j]
		if iv.Completed != nil && jv.Completed != nil {
			return !iv.Completed.After(*jv.Completed)
		} else if iv.Completed != nil {
			return true
		} else if jv.Completed != nil {
			return false
		} else {
			return false
		}
	})
}

func duration(ui Components, dt time.Duration, completed bool) string {
	prec := 1
	sec := dt.Seconds()
	if sec < 10 {
		prec = 2
	} else if sec < 100 {
		prec = 1
	}

	if completed {
		return fmt.Sprintf(ui.DoneDuration, sec, prec)
	} else {
		return fmt.Sprintf(ui.RunningDuration, sec, prec)
	}
}

func (t *trace) printErrorLogs(f io.Writer) {
	for _, v := range t.vertexes {
		if v.Error != "" && !strings.HasSuffix(v.Error, context.Canceled.Error()) {
			fmt.Fprintf(f, t.ui.ErrorHeader, v.Name)
			// tty keeps original logs
			for _, l := range v.logs {
				f.Write(l)
				fmt.Fprintln(f)
			}
			// printer keeps last logs buffer
			if v.logsBuffer != nil {
				for i := 0; i < v.logsBuffer.Len(); i++ {
					if v.logsBuffer.Value != nil {
						fmt.Fprintln(f, string(v.logsBuffer.Value.([]byte)))
					}
					v.logsBuffer = v.logsBuffer.Next()
				}
			}

			if t.ui.ErrorFooter != "" {
				fmt.Fprintf(f, t.ui.ErrorFooter, v.Name)
			}
		}
	}
}

func (t *trace) displayInfo() (d displayInfo) {
	d.startTime = time.Now()
	if t.localTimeDiff != 0 {
		d.startTime = (*t.vertexes[0].Started).Add(t.localTimeDiff)
	}
	d.countTotal = len(t.byDigest)
	for _, v := range t.byDigest {
		if v.Completed != nil {
			d.countCompleted++
		}
	}

	for _, v := range t.vertexes {
		if v.jobCached {
			d.jobs = append(d.jobs, v.jobs...)
			continue
		}
		var jobs []*job
		vertexJob := &job{
			startTime:     addTime(v.Started, t.localTimeDiff),
			completedTime: addTime(v.Completed, t.localTimeDiff),
			name:          strings.Replace(v.Name, "\t", " ", -1),
			vertex:        v,
		}

		if v.Completed == nil {
			vertexJob.name = fmt.Sprintf(t.ui.ConsoleVertexRunning, vertexJob.name)
		} else if v.Error != "" {
			if strings.HasSuffix(v.Error, context.Canceled.Error()) {
				vertexJob.isCanceled = true
				vertexJob.name = fmt.Sprintf(t.ui.ConsoleVertexCanceled, vertexJob.name)
			} else {
				vertexJob.hasError = true
				vertexJob.name = fmt.Sprintf(t.ui.ConsoleVertexErrored, vertexJob.name)
			}
		} else if v.Cached {
			vertexJob.name = fmt.Sprintf(t.ui.ConsoleVertexCached, vertexJob.name)
		} else {
			vertexJob.name = fmt.Sprintf(t.ui.ConsoleVertexDone, vertexJob.name)
		}

		vertexJob.name = v.indent + vertexJob.name
		jobs = append(jobs, vertexJob)
		for _, s := range v.statuses {
			statusJob := &job{
				startTime:     addTime(s.Started, t.localTimeDiff),
				completedTime: addTime(s.Completed, t.localTimeDiff),
				name:          v.indent + fmt.Sprintf(t.ui.ConsoleVertexStatus, s.ID),
			}
			if s.Total != 0 {
				statusJob.status = fmt.Sprintf(
					t.ui.ConsoleVertexStatusProgressBound,
					units.Bytes(s.Current),
					units.Bytes(s.Total),
				)
			} else if s.Current != 0 {
				statusJob.status = fmt.Sprintf(
					t.ui.ConsoleVertexStatusProgressUnbound,
					units.Bytes(s.Current),
				)
			}
			vertexJob.statuses = append(vertexJob.statuses, statusJob)
		}
		d.jobs = append(d.jobs, jobs...)
		v.jobs = jobs
		v.jobCached = true
	}

	return d
}

func split(dt []byte, sep byte, fn func([]byte)) bool {
	if len(dt) == 0 {
		return false
	}
	for {
		if len(dt) == 0 {
			return true
		}
		idx := bytes.IndexByte(dt, sep)
		if idx == -1 {
			fn(dt)
			return false
		}
		fn(dt[:idx])
		dt = dt[idx+1:]
	}
}

func addTime(tm *time.Time, d time.Duration) *time.Time {
	if tm == nil {
		return nil
	}
	t := (*tm).Add(d)
	return &t
}

type display struct {
	ui       Components
	maxWidth int
	repeated bool
}

func (disp *display) status(d displayInfo) string {
	done := d.countCompleted > 0 && d.countCompleted == d.countTotal

	statusFmt := disp.ui.ConsoleRunning
	if done {
		statusFmt = disp.ui.ConsoleDone
	}

	if statusFmt == "" {
		return ""
	}

	return fmt.Sprintf(
		statusFmt,
		duration(disp.ui, time.Since(d.startTime), done),
		d.countCompleted,
		d.countTotal,
	)
}

func (disp *display) print(w io.Writer, d displayInfo, termHeight, width, height int) {
	for _, j := range d.jobs {
		disp.printJob(w, j, d, termHeight, width, height)
	}
}

func (disp *display) printJob(w io.Writer, j *job, d displayInfo, termHeight, width, height int) {
	endTime := time.Now()
	if j.completedTime != nil {
		endTime = *j.completedTime
	}

	if j.startTime == nil {
		return
	}

	if strings.Contains(j.name, HideTag) {
		return
	}

	out := j.name
	if j.status != "" {
		out += " " + j.status
	}

	dt := endTime.Sub(*j.startTime).Truncate(time.Millisecond)
	out += " " + duration(disp.ui, dt, j.completedTime != nil)

	fmt.Fprintf(w, "%s\n", out)

	for _, s := range j.statuses {
		disp.printJob(
			w,
			s,
			d,
			termHeight,
			width,
			height,
		)
	}

	if j.vertex != nil && j.vertex.termBytes > 0 {
		term := j.vertex.term
		term.Resize(termHeight, width-termPad)
		renderTerm(w, disp.ui, term)
	}
}

func renderTerm(w io.Writer, ui Components, term *vt100.VT100) {
	used := term.UsedHeight()

	for row, l := range term.Content {
		if row+1 > used {
			break
		}

		var lastFormat vt100.Format

		var line string
		for col, r := range l {
			f := term.Format[row][col]

			if f != lastFormat {
				lastFormat = f
				line += renderFormat(f)
			}

			line += string(r)
		}

		line += aec.Reset

		out := fmt.Sprintf(ui.ConsoleLogFormat, line)
		fmt.Fprintf(w, "%s\n", out)
	}
}

func renderFormat(f vt100.Format) string {
	if f == (vt100.Format{}) {
		return aec.Reset
	}

	b := aec.EmptyBuilder

	switch f.Fg {
	case vt100.Black:
		b = b.BlackF()
	case vt100.Red:
		b = b.RedF()
	case vt100.Green:
		b = b.GreenF()
	case vt100.Yellow:
		b = b.YellowF()
	case vt100.Blue:
		b = b.BlueF()
	case vt100.Magenta:
		b = b.MagentaF()
	case vt100.Cyan:
		b = b.CyanF()
	case vt100.White:
		b = b.WhiteF()
	}

	switch f.Bg {
	case vt100.Black:
		b = b.BlackB()
	case vt100.Red:
		b = b.RedB()
	case vt100.Green:
		b = b.GreenB()
	case vt100.Yellow:
		b = b.YellowB()
	case vt100.Blue:
		b = b.BlueB()
	case vt100.Magenta:
		b = b.MagentaB()
	case vt100.Cyan:
		b = b.CyanB()
	case vt100.White:
		b = b.WhiteB()
	}

	switch f.Intensity {
	case vt100.Bright:
		b = b.Bold()
	case vt100.Dim:
		b = b.Faint()
	}

	return b.ANSI.String()
}

func nonAnsiLen(s string) int {
	l := 0

	var inAnsi bool
	for _, c := range s {
		if inAnsi {
			if c == 'm' {
				inAnsi = false
			}

			continue
		}

		if c == '\x1b' {
			inAnsi = true
			continue
		}

		l++
	}

	return l
}
