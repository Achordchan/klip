package prefswin

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"

	"klip/internal/panel"
	"klip/internal/prefs"
)

// Size of the window, which cannot be resized (PreferencesWindow.axaml).
const Width, Height = 460, 560

const (
	labelWidth = 120
	toastFor   = 1100 * time.Millisecond
)

// View is the window's content.
func (m *Model) View(c *ui.Context) {
	p := panel.PaletteFor(c.Theme().Dark)
	c.Root().Background(p.Background)
	m.followLogin()
	m.followSettings()
	for _, msg := range m.takeToasts() {
		m.toastMsg, m.toastUntil = msg, c.Now().Add(toastFor)
	}
	if !m.later.IsZero() {
		if left := m.later.Sub(c.Now()); left <= 0 {
			m.later = time.Time{}
			m.toastMsg, m.toastUntil = "设置已生效", c.Now().Add(toastFor)
		} else {
			c.After(left)
		}
	}
	if m.toastMsg != "" {
		if left := m.toastUntil.Sub(c.Now()); left > 0 {
			c.After(left)
		} else {
			m.toastMsg = ""
		}
	}
	c.OnShortcut(0, ui.KeyEscape, func() {
		if m.forgot || m.change {
			m.closeDialogs()
		}
	})

	ui.Box(c).Fill().Children(func() {
		ui.Column(c).Fill().Padding(14).Background(p.Background).Children(func() {
			ui.Tabs(c, &m.tab, "通用", "内容", "货架", "云同步").TextColor(p.Text)
			ui.Scroll(c).Grow(1).Children(func() {
				ui.Column(c).Padding(10, 4, 0).Gap(12).Children(func() {
					switch m.tab {
					case 0:
						m.general(c, p)
					case 1:
						m.content(c, p)
					case 2:
						m.shelf(c, p)
					default:
						m.cloud(c, p)
					}
				})
			})
			ui.Box(c).FillWidth().Height(1).Background(p.Divider).Margin(10, 0, 0)
			ui.Row(c).FillWidth().Margin(10, 0, 0).Justify(ui.End).Children(func() {
				if panel.Button(c, p, "关闭", 5, 11, 14).Label("关闭").Clicked() {
					m.Host.Close()
				}
			})
		})
		m.passwordDialog(c, p)
		if m.toastMsg != "" {
			ui.Row(c).Absolute().Left(0).Right(0).Bottom(12).Justify(ui.Center).PassThrough().Children(func() {
				ui.Row(c).Padding(8, 12).Radius(8).Background(p.ToastBg).Children(func() {
					ui.Text(c, m.toastMsg).FontSize(14).TextColor(p.ToastText)
				})
			})
		}
	})
}

// effect turns what a setter did into toasts and box texts. Numbers typed
// toast after a pause, the rest at once.
func (m *Model) effect(c *ui.Context, e prefs.Effect, typed bool, box *string) {
	if e.SetText && box != nil {
		*box = e.Text
	}
	if e.Err != nil {
		m.toast(e.Err.Error())
		return
	}
	if !e.Changed {
		return
	}
	if typed {
		m.later = c.Now().Add(700 * time.Millisecond)
		c.After(700 * time.Millisecond)
		return
	}
	m.toast("设置已生效")
}

func (m *Model) check(c *ui.Context, p Palette, label string, on *bool, set func(bool) prefs.Effect) {
	if ui.Checkbox(c, on, label).TextColor(p.Text).Changed() {
		e := set(*on)
		if e.Err != nil && !e.Changed {
			*on = !*on // the system refused: show what is true
		}
		// (A setting that took effect but could not be saved stays as it is:
		// it is on for now, and the message says it will not survive a restart.)
		m.effect(c, e, false, nil)
	}
}

// Palette is the colours of the windows.
type Palette = panel.Palette

// row is a label and a control side by side (StackPanel, Horizontal).
func row(c *ui.Context, p Palette, label string, muted bool, body func()) {
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		col := p.Text
		if muted {
			col = p.Muted
		}
		ui.Text(c, label).Width(labelWidth).TextColor(col)
		body()
	})
}

func heading(c *ui.Context, p Palette, s string, weight int) {
	ui.Text(c, s).FontWeight(weight).TextColor(p.Text)
}

// field is a TextBox: a bordered box with the text inside, over the colour
// of the control or of the window.
func field(c *ui.Context, p Palette, value *string, label, hint string, width float32, over ui.Color, password bool) ui.Element {
	var in ui.Element
	box := ui.Row(c).Height(32).Padding(0, 10).AlignItems(ui.Center).Background(over).Border(1, p.Border).Radius(4)
	if width > 0 {
		box.Width(width)
	} else {
		box.FillWidth()
	}
	box.Children(func() {
		in = ui.TextInputBase(c, value).Grow(1).FontSize(14).TextColor(p.Text).Label(label).Placeholder(hint)
		if password {
			in.Password()
		}
	})
	return in
}

func optionNames(list []prefs.Option) []string {
	out := make([]string, len(list))
	for i, o := range list {
		out[i] = o.Display
	}
	return out
}

func byDisplay(list []prefs.Option, display string) prefs.Option {
	for _, o := range list {
		if o.Display == display {
			return o
		}
	}
	return list[0]
}

func (m *Model) general(c *ui.Context, p Palette) {
	m.check(c, p, "开机启动", &m.v.StartWithWindows, m.Prefs.SetStartWithWindows)
	row(c, p, "主题", false, func() {
		sel := m.v.Theme.Display
		if ui.Select(c, &sel, optionNames(prefs.Themes)).Label("主题选择").Width(180).Changed() {
			m.v.Theme = byDisplay(prefs.Themes, sel)
			m.effect(c, m.Prefs.SetTheme(m.v.Theme.Value), false, nil)
		}
	})
	row(c, p, "检查更新", false, func() {
		if panel.Button(c, p, "检查", 5, 11, 14).Label("检查更新").Clicked() {
			if check := m.Host.CheckUpdates(); check != nil {
				check()
			}
		}
	})
}

func (m *Model) content(c *ui.Context, p Palette) {
	heading(c, p, "历史", 700)
	m.check(c, p, "合并重复内容", &m.v.MergeDuplicates, m.Prefs.SetMergeDuplicates)
	m.check(c, p, "收藏项不计入上限", &m.v.ExcludePinnedFromLimits, m.Prefs.SetExcludePinnedFromLimits)
	row(c, p, "最大条目", false, func() {
		if field(c, p, &m.v.MaxItems, "最大条目数", "", 100, p.Control, false).Changed() {
			m.effect(c, m.Prefs.SetMaxItems(m.v.MaxItems), true, nil)
		}
	})
	row(c, p, "最大容量（MB）", false, func() {
		if field(c, p, &m.v.MaxMegabytes, "最大容量 MB", "", 100, p.Control, false).Changed() {
			m.effect(c, m.Prefs.SetMaxMegabytes(m.v.MaxMegabytes), true, &m.v.CaptureFileMaxMegabytes)
		}
	})
	row(c, p, "存储位置", false, func() {
		if panel.Button(c, p, "打开", 5, 11, 14).Label("打开存储位置").Clicked() {
			m.Host.OpenStorage()
		}
	})
	ui.Box(c).FillWidth().Height(1).Background(p.Divider).Margin(4, 0)
	heading(c, p, "捕获", 700)
	m.check(c, p, "捕获文本", &m.v.CaptureText, m.Prefs.SetCaptureText)
	m.check(c, p, "捕获图片", &m.v.CaptureImages, m.Prefs.SetCaptureImages)
	m.check(c, p, "捕获文件", &m.v.CaptureFiles, m.Prefs.SetCaptureFiles)
	row(c, p, "文件后缀", false, func() {
		if field(c, p, &m.v.CaptureFileExtensions, "文件后缀列表", ".pdf,.docx,.txt（留空=全部）", 240, p.Control, false).Changed() {
			m.effect(c, m.Prefs.SetCaptureFileExtensions(m.v.CaptureFileExtensions), true, nil)
		}
	})
	row(c, p, "单文件上限（MB）", false, func() {
		if field(c, p, &m.v.CaptureFileMaxMegabytes, "单文件上限 MB", "20", 100, p.Control, false).Changed() {
			m.effect(c, m.Prefs.SetCaptureFileMax(m.v.CaptureFileMaxMegabytes), true, &m.v.CaptureFileMaxMegabytes)
		}
	})
}

func (m *Model) shelf(c *ui.Context, p Palette) {
	m.check(c, p, "启用货架", &m.v.ShelfEnabled, m.Prefs.SetShelfEnabled)
	if !m.v.ShelfEnabled {
		return
	}
	row(c, p, "触发键", false, func() {
		sel := m.v.ShelfModifier.Display
		if ui.Select(c, &sel, optionNames(prefs.ShelfModifiers)).Label("触发键选择").Width(220).Changed() {
			m.v.ShelfModifier = byDisplay(prefs.ShelfModifiers, sel)
			m.effect(c, m.Prefs.SetShelfModifier(m.v.ShelfModifier.Value), false, nil)
		}
	})
	ui.Row(c).FillWidth().Padding(10).Background(p.Control).Border(1, p.Border).Radius(10).Children(func() {
		ui.Text(c, "按住触发键，在任意位置按住鼠标左键并快速左右摇晃，即可唤出货架窗口。").
			FontSize(12).TextColor(p.Muted).Wrap().FillWidth()
	})
}

func (m *Model) card(c *ui.Context, p Palette, body func()) {
	ui.Column(c).FillWidth().Padding(12).Gap(10).Background(p.Control).Border(1, p.Border).Radius(10).Children(body)
}

func (m *Model) cloud(c *ui.Context, p Palette) {
	ui.Column(c).FillWidth().Gap(16).Children(func() {
		ui.Column(c).FillWidth().Gap(8).Children(func() {
			heading(c, p, "账户", 600)
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Start).Children(func() {
				ui.Text(c, "登录状态").Width(labelWidth).TextColor(p.Muted)
				ui.Column(c).Grow(1).MinWidth(0).Gap(8).Children(func() {
					ui.Text(c, m.statusText).TextColor(p.Text).Wrap()
					if !m.signedIn {
						m.card(c, p, func() {
							field(c, p, &m.email, "邮箱地址", "请输入邮箱", 0, p.Background, false)
							in := field(c, p, &m.password, "密码输入", "请输入密码", 0, p.Background, true)
							if in.Submitted() {
								m.signIn(false)
							}
							ui.Row(c).Gap(8).Children(func() {
								if panel.Button(c, p, "登录", 6, 12, 14).Disabled(m.authBusy).Label("登录").Clicked() {
									m.signIn(false)
								}
								if panel.Button(c, p, "注册", 6, 12, 14).Disabled(m.authBusy).Label("注册").Clicked() {
									m.signIn(true)
								}
								if panel.Button(c, p, "忘记密码", 6, 12, 14).Label("忘记密码").Clicked() {
									m.openForgot()
								}
							})
						})
					} else {
						ui.Row(c).Gap(8).Children(func() {
							if panel.Button(c, p, "退出登录", 6, 12, 14).Label("退出登录").Clicked() {
								m.signOut()
							}
							if panel.Button(c, p, "修改密码", 6, 12, 14).Label("修改密码").Clicked() {
								m.openChange()
							}
						})
					}
				})
			})
		})

		if !m.signedIn {
			ui.Row(c).FillWidth().Padding(12).Background(p.Control).Border(1, p.Border).Radius(10).Children(func() {
				ui.Text(c, "登录后即可启用云同步，剪贴板历史将自动同步到云端。").TextColor(p.Muted).Wrap().FillWidth()
			})
			return
		}

		ui.Column(c).FillWidth().Gap(8).Children(func() {
			heading(c, p, "同步", 600)
			row(c, p, "手动操作", true, func() {
				if panel.Button(c, p, "立即同步", 6, 12, 14).Label("立即同步").Clicked() {
					m.Host.SyncNow()
				}
				// How it went: the toast is in the main window, which is hidden
				// while this one is open.
				if m.SyncStatus != nil {
					ui.Text(c, m.SyncStatus()).TextColor(p.Muted).FontSize(12).Wrap()
				}
			})
		})

		ui.Column(c).FillWidth().Gap(8).Children(func() {
			heading(c, p, "订阅", 600)
			row(c, p, "订阅状态", true, func() {
				ui.Text(c, m.sub.Status).TextColor(p.Text)
				if panel.Button(c, p, "刷新", 4, 8, 14).Label("刷新").Clicked() {
					m.loadSubscription()
				}
			})
			row(c, p, "套餐", true, func() { ui.Text(c, m.sub.Tier).TextColor(p.Text) })
			row(c, p, "存储占用", true, func() { ui.Text(c, m.sub.Storage).TextColor(p.Text) })
			row(c, p, "保留天数", true, func() { ui.Text(c, m.sub.Retention).TextColor(p.Text) })
			row(c, p, "超限状态", true, func() {
				col := p.Text
				if m.sub.OverLimit {
					col = p.Red
				}
				ui.Text(c, m.sub.Limit).TextColor(col)
			})
			row(c, p, "兑换卡密", true, func() {
				in := field(c, p, &m.redeem, "卡密输入", "输入卡密", 160, p.Control, false)
				if panel.Button(c, p, "兑换", 6, 12, 14).Disabled(m.redeemBusy).Label("兑换").Clicked() || in.Submitted() {
					m.redeemCard()
				}
			})
		})
	})
}

func (m *Model) passwordDialog(c *ui.Context, p Palette) {
	if !m.forgot && !m.change {
		return
	}
	label := func(s string) { ui.Text(c, s).FontSize(12).TextColor(p.Muted) }
	// A dialog in its own right: built over the window and Modal, so that the
	// focus goes into it, Tab goes round inside it, and what is behind it (the
	// sign-in form, whose Enter signs in) takes no keys while it shows.
	ui.Overlay(c, func() {
		back := ui.Column(c).Absolute().Left(0).Right(0).Top(0).Bottom(0).Background(ui.RGBA(0, 0, 0, 0.6)).Center().Modal()
		if back.OverlayShortcut(0, ui.KeyEscape) {
			m.closeDialogs()
		}
		back.Children(func() {
			m.passwordForm(c, p, label)
		})
	})
}

// passwordForm is the box of the password dialog.
func (m *Model) passwordForm(c *ui.Context, p Palette, label func(string)) {
	ui.Column(c).Width(392).Padding(18).Gap(12).Background(p.Panel).Border(1, p.Border).Radius(16).Children(func() {
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			title := "找回密码"
			if m.change {
				title = "修改密码"
			}
			ui.Text(c, title).FontSize(18).FontWeight(600).TextColor(p.Text)
			ui.Text(c, "验证码会发送到你的邮箱，修改成功后需要重新登录。").TextColor(p.Muted).Wrap()
		})
		ui.Column(c).FillWidth().Gap(10).Children(func() {
			if m.forgot {
				ui.Column(c).FillWidth().Gap(5).Children(func() {
					label("邮箱")
					field(c, p, &m.email, "找回邮箱", "请输入邮箱", 0, p.Background, false)
				})
			} else {
				ui.Column(c).FillWidth().Gap(5).Children(func() {
					label("当前账号")
					ui.Row(c).FillWidth().Padding(8, 10).Background(p.Background).Border(1, p.Border).Radius(8).Children(func() {
						ui.Text(c, m.statusText).TextColor(p.Text)
					})
				})
			}
			ui.Column(c).FillWidth().Gap(5).Children(func() {
				label("验证码")
				ui.Row(c).FillWidth().Gap(8).Children(func() {
					ui.Row(c).Grow(1).Children(func() {
						field(c, p, &m.code, "验证码输入", "6 位邮箱验证码", 0, p.Background, false)
					})
					text := "发送验证码"
					if m.cooldown > 0 {
						text = fmt.Sprintf("%ds 后重发", m.cooldown)
					}
					if panel.Button(c, p, text, 6, 12, 14).Disabled(m.codeBusy || m.cooldown > 0).Label(text).Clicked() {
						m.sendCode()
					}
				})
			})
			ui.Column(c).FillWidth().Gap(5).Children(func() {
				label("新密码")
				field(c, p, &m.pw1, "新密码输入", "至少 8 位", 0, p.Background, true)
			})
			ui.Column(c).FillWidth().Gap(5).Children(func() {
				label("确认新密码")
				field(c, p, &m.pw2, "确认新密码输入", "再次输入新密码", 0, p.Background, true)
			})
		})
		ui.Box(c).FillWidth().Height(1).Background(p.Divider).Margin(6, 0, 0)
		ui.Row(c).FillWidth().Gap(8).Justify(ui.End).Children(func() {
			if panel.Button(c, p, "取消", 6, 12, 14).Label("取消").Clicked() {
				m.closeDialogs()
			}
			ok := "确认修改"
			if m.forgot {
				ok = "重置密码"
			}
			if ui.PrimaryButton(c, ok).Disabled(m.codeBusy).Clicked() {
				m.submitPassword()
			}
		})
	})
}
