package previewwin

import "github.com/egoist/mygo/ui"

// loadIcon starts at most one shell lookup per manager. It is called with mu
// held; slow network paths never block rendering or create unbounded workers.
func (mg *Manager) loadIcon(path string) {
	if path == "" || mg.IconOf == nil || mg.iconBusy {
		return
	}
	if _, ok := mg.icons[path]; ok {
		return
	}
	mg.iconBusy = true
	lookup := mg.IconOf
	go func() {
		icon := lookup(path)
		mg.runOnMain(func() {
			mg.mu.Lock()
			defer mg.mu.Unlock()
			mg.iconBusy = false
			if mg.shut {
				return
			}
			if len(mg.icons) >= 64 {
				mg.icons = nil
			}
			if mg.icons == nil {
				mg.icons = make(map[string]*ui.Bitmap)
			}
			mg.icons[path] = icon
			if mg.model != nil {
				current := ""
				if mg.model.Item.SourceAppPath != nil {
					current = *mg.model.Item.SourceAppPath
				}
				if current == path {
					mg.model.icon = icon
					if mg.win != nil {
						mg.win.Update(func() {})
					}
				} else {
					mg.loadIcon(current)
				}
			}
		})
	}()
}
