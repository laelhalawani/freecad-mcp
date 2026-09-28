package main

// The interactive app opens when the binary is run bare in a terminal. It
// starts as a menu. Menu items either run a report into a scrollable detail
// view (doctor, addon, connection) or open an interactive page (Share this
// PC in app_share.go, Connect in app_connect.go).

import (
	"bytes"
	"context"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"
	"github.com/sairaph/mcp-wizard/app/detail"
	"github.com/sairaph/mcp-wizard/app/menu"
	"github.com/sairaph/mcp-wizard/async"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

const (
	stepMenu app.Step = iota
	stepDoctor
	stepPage
)

// appPage is an interactive page of the app. It gets every message while it
// is open (after ctrl+c, which always quits) and reports done when the user
// leaves it (esc), which returns to the menu.
type appPage interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (done bool, cmd tea.Cmd)
	View() string
}

type appState struct {
	app.AppModel
	ctx    context.Context
	menu   *menu.Model
	detail *detail.Model
	page   appPage
}

func runApp(ctx context.Context) int {
	s := &appState{ctx: ctx}
	s.menu = menu.New(domain.BinaryName+" "+version, func() []menu.Item {
		return []menu.Item{
			{Label: "Run doctor", Action: "doctor"},
			{Label: "Install or update the FreeCAD addon", Action: "addon"},
			{Label: "Check the FreeCAD connection", Action: "connection"},
			{Label: "Share this PC (remote access)", Action: "share"},
			{Label: "Connect to FreeCAD on another computer", Action: "connect"},
			{Label: "Quit", Action: "quit"},
		}
	})
	return app.Run(ctx, s, app.Options{Title: domain.BinaryName, Version: version})
}

func (m *appState) Init() tea.Cmd { return m.menu.Init() }

func (m *appState) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handled, cmd := m.HandleGlobalKeys(msg); handled {
		return m, cmd
	}

	if m.Step == stepPage && m.page != nil {
		done, cmd := m.page.Update(msg)
		if done {
			m.Step = stepMenu
			m.page = nil
		}
		return m, cmd
	}

	switch msg := msg.(type) {
	case app.ActionMsg:
		switch msg.Source {
		case "menu":
			action, _ := msg.Data.(string)
			if msg.Value == "quit" || action == "quit" {
				m.Quit = true
				return m, tea.Quit
			}
			switch action {
			case "share":
				return m.openPage(newSharePage(m.ctx))
			case "connect":
				return m.openPage(newConnectPage(m.ctx))
			}
			var (
				title string
				work  func(io.Writer)
			)
			switch action {
			case "doctor":
				title, work = "Doctor", func(w io.Writer) { newDoctor(m.ctx).Run(m.ctx, w) }
			case "addon":
				title, work = "FreeCAD addon", func(w io.Writer) { installAddonReport(m.ctx, w, nil, false) }
			case "connection":
				title, work = "FreeCAD connection", func(w io.Writer) { connectionReport(m.ctx, w) }
			}
			if work != nil {
				m.Step = stepDoctor
				m.Status = "Working..."
				m.detail = detail.New(title, m.Status)
				return m, tea.Batch(m.detail.Init(), async.Load(func() (string, error) {
					var buf bytes.Buffer
					work(&buf)
					return buf.String(), nil
				}))
			}
		case "detail":
			if msg.Value == "back" {
				m.Step = stepMenu
				return m, nil
			}
		}
		return m, nil

	case async.Result[string]:
		m.Status = ""
		if msg.Err != nil {
			m.detail.SetContent("Failed: " + msg.Err.Error())
		} else {
			m.detail.SetContent(msg.Value)
		}
		return m, nil
	}

	switch m.Step {
	case stepMenu:
		return m, m.menu.Update(msg)
	case stepDoctor:
		if m.detail != nil {
			return m, m.detail.Update(msg)
		}
	}
	return m, nil
}

// openPage shows page until it reports done.
func (m *appState) openPage(page appPage) (tea.Model, tea.Cmd) {
	m.page = page
	m.Step = stepPage
	return m, page.Init()
}

func (m *appState) View() string {
	switch m.Step {
	case stepDoctor:
		if m.detail != nil {
			return m.detail.View()
		}
	case stepPage:
		if m.page != nil {
			return m.page.View()
		}
	}
	return m.menu.View()
}
