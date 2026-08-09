package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtinput"
	"github.com/unxed/vtui"
)

type FoundFile struct {
	Path string
	Item vfs.VFSItem
}

// maxRemoteFindResults caps what a remote search brings back in one answer.
// The local walk has no such limit because it costs nothing to keep going;
// a remote one pays for every hit on the wire.
const maxRemoteFindResults = 10000

// ExecuteFindFile initiates a background search and displays a progress dialog.
func ExecuteFindFile(pf *PanelsFrame, v vfs.VFS, startDir, mask, text string) {
	dlg := vtui.NewCenteredDialog(60, 9, Msg("FindFile.SearchingTitle"))
	dlg.AttentionSuppressed = true

	lblMask := vtui.NewLabel(0, 0, Msg("FindFile.MaskPrompt")+" "+mask, nil)
	lblDir := vtui.NewLabel(0, 0, Msg("FindFile.Scanning")+" ...", nil)
	lblFound := vtui.NewLabel(0, 0, fmt.Sprintf(Msg("FindFile.FoundCount"), 0), nil)

	btnCancel := vtui.NewButton(0, 0, Msg("vtui.Cancel"))

	dlg.AddItem(lblMask)
	dlg.AddItem(lblDir)
	dlg.AddItem(lblFound)
	dlg.AddItem(btnCancel)

	vbox := vtui.NewVBoxLayout(dlg.X1+2, dlg.Y1+2, 60-4, 9-4)
	vbox.Add(lblMask, vtui.Margins{}, vtui.AlignLeft)
	vbox.Add(lblDir, vtui.Margins{Top: 1}, vtui.AlignLeft)

	hbox := vtui.NewHBoxLayout(0, 0, 60-4, 1)
	hbox.Add(lblFound, vtui.Margins{}, vtui.AlignLeft)
	hbox.Add(btnCancel, vtui.Margins{}, vtui.AlignRight)
	vbox.Add(hbox, vtui.Margins{Top: 1}, vtui.AlignFill)
	vbox.Apply()

	var taskCtx *vtui.TaskContext
	btnCancel.OnClick = func() {
		dlg.SetExitCode(1)
	}
	dlg.OnResult = func(code int) {
		if taskCtx != nil {
			taskCtx.Cancel()
		}
	}

	// Since we are inside an action handler (UI thread), we can push directly
	vtui.FrameManager.AddScreenHeadless(dlg)

	taskCtx = vtui.RunAsync(func(ctx *vtui.TaskContext) {
		// Parse masks (e.g. "*.go, *.txt")
		masks := strings.Split(mask, ",")
		for i := range masks {
			masks[i] = strings.TrimSpace(masks[i])
			// Far compatibility: *.* translates to * in filepath.Match logic
			masks[i] = strings.ReplaceAll(masks[i], "*.*", "*")
		}
		if len(masks) == 0 || mask == "" {
			masks = []string{"*"}
		}

		searchTextLower := strings.ToLower(text)
		var found []FoundFile
		var lastUpdate time.Time // Used for throttling UI redraws
		// A remote finder that supports progress reports intermediate
		// counters before we get the entries themselves: what has been
		// scanned so far and how many have matched. During the walk
		// found is still empty on our side, so the "Found:" line has to
		// prefer the reported number until the final answer lands.
		// remotePath is what a remote progress last reported as the
		// head of the walk; the "final" updateUI at the end otherwise
		// reverts the label to startDir and loses the last frame.
		var remoteFound int64
		var remotePath string

		updateUI := func(dir string, force bool) {
			now := time.Now()
			if force || now.Sub(lastUpdate) > 50*time.Millisecond {
				lastUpdate = now
				currentCount := int64(len(found))
				if remoteFound > currentCount {
					currentCount = remoteFound
				}
				showDir := dir
				if remotePath != "" {
					showDir = remotePath
				}
				displayDir := runewidth.Truncate(showDir, 56, "...")
				ctx.RunOnUI(func() {
					lblDir.SetText(Msg("FindFile.Scanning") + " " + displayDir)
					lblFound.SetText(fmt.Sprintf(Msg("FindFile.FoundCount"), currentCount))
					vtui.FrameManager.Redraw()
				})
			}
		}

		var walk func(dir string) error
		walk = func(dir string) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			updateUI(dir, false)

			return v.ReadDir(ctx.Context, dir, func(chunk []vfs.VFSItem) {
				for _, item := range chunk {
					if ctx.Err() != nil {
						return
					}
					if item.Name == ".." {
						continue
					}

					itemPath := v.Join(dir, item.Name)

					if item.IsDir {
						_ = walk(itemPath) // Ignore permissions/read errors to continue walking
					} else {
						// 1. Check Mask
						matched := false
						for _, m := range masks {
							if m == "" {
								continue
							}
							match, _ := filepath.Match(m, item.Name)
							if match {
								matched = true
								break
							}
						}
						if !matched {
							continue
						}

						// 2. Check Text Content
						if text != "" {
							if !fileContainsText(ctx.Context, v, itemPath, searchTextLower) {
								continue
							}
						}

						// 3. Register Hit
						found = append(found, FoundFile{Path: itemPath, Item: item})
						updateUI(dir, false)
					}
				}
			})
		}

		// A file system that can search its own tree does the walking there:
		// one request instead of a round trip per directory, and a remote
		// grep instead of downloading every candidate only to reject it.
		var err error
		searched := false
		if finder, ok := v.(vfs.FileFinder); ok {
			updateUI(startDir, true)
			hits, findErr := finder.FindFiles(ctx.Context, startDir, vfs.FindQuery{
				Masks:      masks,
				Text:       text,
				IgnoreCase: true,
				Limit:      maxRemoteFindResults,
				// A remote finder that supports progress reports the
				// last path it visited and running counters. Force the
				// redraw (helper's own P cadence is 300 ms, well above
				// the client-side throttle, so nothing to save here)
				// and remember the path so the final updateUI at the
				// end does not revert the label back to startDir.
				Progress: func(p vfs.FindProgress) {
					remoteFound = p.Found
					if p.Path != "" {
						remotePath = p.Path
					}
					updateUI(p.Path, true)
				},
			})
			if findErr == nil {
				for _, hit := range hits {
					found = append(found, FoundFile{Path: hit.Path, Item: hit.Item})
				}
				searched = true
			} else if ctx.Err() == nil {
				vtui.DebugLog("FIND: remote search unavailable, walking instead: %v", findErr)
			}
		}
		if !searched {
			err = walk(startDir)
		}
		updateUI(startDir, true) // Guarantee final state rendering

		ctx.RunOnUI(func() {
			dlg.Close()
			if err != nil && err != context.Canceled {
				vtui.ShowMessage(" Error ", fmt.Sprintf("Search failed:\n%v", err), []string{"&Ok"})
			} else if len(found) == 0 {
				vtui.ShowMessage(" Find File ", "File not found.", []string{"&Ok"})
			} else {
				ShowSearchResults(pf, v, found)
			}
		})
	})
}

// fileContainsText scans a file for a substring using chunked reads.
// It handles overlaps to ensure words crossing chunk boundaries are found.
func fileContainsText(ctx context.Context, v vfs.VFS, path string, textLower string) bool {
	f, err := v.Open(ctx, path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 128*1024) // 128KB chunks
	overlap := len(textLower) - 1
	if overlap < 0 {
		overlap = 0
	}

	var tail []byte

	for {
		if ctx.Err() != nil {
			return false
		}

		n, err := f.Read(ctx, buf)
		if n > 0 {
			data := buf[:n]

			// Prepend tail from previous chunk
			if len(tail) > 0 {
				data = append(tail, data...)
			}

			if strings.Contains(strings.ToLower(string(data)), textLower) {
				return true
			}

			// Save the tail for the next overlap
			if len(data) > overlap {
				// Append to nil to force a new allocation, avoiding memory pinning
				tail = append([]byte(nil), data[len(data)-overlap:]...)
			} else {
				tail = append([]byte(nil), data...)
			}
		}
		if err != nil {
			break // EOF or error
		}
	}
	return false
}

type foundFileRow struct {
	ff FoundFile
	v  vfs.VFS
}

func (r foundFileRow) GetCellText(col int) string {
	switch col {
	case 0:
		return r.ff.Item.Name
	case 1:
		if r.ff.Item.IsDir {
			return "<DIR>"
		}
		return fmt.Sprintf("%d", r.ff.Item.Size)
	case 2:
		return r.v.Dir(r.ff.Path)
	}
	return ""
}

type SearchResultsWindow struct {
	vtui.Window
	table *vtui.Table
	found []FoundFile
	vfs   vfs.VFS
	pf    *PanelsFrame
}

func (srw *SearchResultsWindow) ProcessKey(e *vtinput.InputEvent) bool {
	if !e.KeyDown {
		return srw.Window.ProcessKey(e)
	}

	switch e.VirtualKeyCode {
	case vtinput.VK_F3:
		return srw.HandleCommand(CmView, nil)
	case vtinput.VK_F4:
		return srw.HandleCommand(CmEdit, nil)
	}

	return srw.Window.ProcessKey(e)
}

func (srw *SearchResultsWindow) HandleCommand(cmd int, args any) bool {
	idx := srw.table.SelectPos
	if idx >= 0 && idx < len(srw.found) {
		ff := srw.found[idx]
		switch cmd {
		case CmView:
			actionOpenViewer(srw.pf, srw.vfs, ff.Path)
			return true
		case CmEdit:
			actionOpenEditor(srw.pf, srw.vfs, ff.Path)
			return true
		}
	}
	return srw.Window.HandleCommand(cmd, args)
}

func (srw *SearchResultsWindow) GetKeyLabels() *vtui.KeySet {
	return &vtui.KeySet{
		Normal: vtui.KeyBarLabels{
			"", "", "View", "Edit", "", "", "", "", "", "Quit", "", "",
		},
	}
}

func ShowSearchResults(pf *PanelsFrame, v vfs.VFS, found []FoundFile) {
	dlgW, dlgH := 76, 20
	baseDlg := vtui.NewCenteredDialog(dlgW, dlgH, Msg("FindFile.SearchResultsTitle"))

	srw := &SearchResultsWindow{
		Window: *baseDlg,
		found:  found,
		vfs:    v,
		pf:     pf,
	}

	cols := []vtui.TableColumn{
		{Title: "Name", Width: 20},
		{Title: "Size", Width: 10, Alignment: vtui.AlignRight},
		{Title: "Path", Width: 38},
	}
	srw.table = vtui.NewTable(0, 0, 72, 12, cols)
	srw.table.SetOwner(srw) // Explicit owner for command routing
	srw.table.ShowScrollBar = true

	rows := make([]vtui.TableRow, len(found))
	for i, ff := range found {
		rows[i] = foundFileRow{ff, v}
	}
	srw.table.SetRows(rows)

	btnGo := vtui.NewButton(0, 0, Msg("FindFile.BtnGoTo"))
	btnGo.SetOwner(srw)
	btnView := vtui.NewButton(0, 0, Msg("FindFile.BtnView"))
	btnView.SetOwner(srw)
	btnEdit := vtui.NewButton(0, 0, Msg("FindFile.BtnEdit"))
	btnEdit.SetOwner(srw)
	btnClose := vtui.NewButton(0, 0, Msg("FindFile.BtnClose"))
	btnClose.SetOwner(srw)

	btnGo.IsDefault = true

	doGoTo := func() {
		idx := srw.table.SelectPos
		if idx >= 0 && idx < len(found) {
			ff := found[idx]
			srw.Close()
			if fsp := pf.getActivePanel(); fsp != nil {
				fsp.vfs.SetPath(v.Dir(ff.Path))
				fsp.pendingSelection = v.Base(ff.Path)
				fsp.ReadDirectory()
				pf.showPanels = true
			}
		}
	}

	srw.table.OnAction = func(idx int) { doGoTo() }
	btnGo.OnClick = doGoTo
	btnClose.OnClick = func() { srw.Close() }
	btnView.OnClick = func() { srw.HandleCommand(CmView, nil) }
	btnEdit.OnClick = func() { srw.HandleCommand(CmEdit, nil) }

	vbox := vtui.NewVBoxLayout(srw.X1+2, srw.Y1+2, dlgW-4, dlgH-4)
	vbox.Add(srw.table, vtui.Margins{Bottom: 1}, vtui.AlignFill)

	hbox := vtui.NewHBoxLayout(0, 0, dlgW-4, 1)
	hbox.HorizontalAlign = vtui.AlignCenter
	hbox.Spacing = 2
	hbox.Add(btnGo, vtui.Margins{}, vtui.AlignTop)
	hbox.Add(btnView, vtui.Margins{}, vtui.AlignTop)
	hbox.Add(btnEdit, vtui.Margins{}, vtui.AlignTop)
	hbox.Add(btnClose, vtui.Margins{}, vtui.AlignTop)

	vbox.Add(hbox, vtui.Margins{}, vtui.AlignFill)
	vbox.Apply()

	srw.AddItem(srw.table)
	srw.AddItem(btnGo)
	srw.AddItem(btnView)
	srw.AddItem(btnEdit)
	srw.AddItem(btnClose)

	vtui.FrameManager.Push(srw)
}
