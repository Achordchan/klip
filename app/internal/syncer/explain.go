package syncer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"klip/internal/syncclient"
)

// Where an error is shown decides some of its words: the main window's
// background sync and the preferences' buttons say slightly different things,
// as the two C# explain functions do.
type Where int

const (
	InMainWindow Where = iota
	InPreferences
)

// Explain gives the text to show the user for a failed sync, in Chinese as
// the C# client does.
func Explain(err error, where Where) string {
	msg := strings.TrimSpace(err.Error())

	if errors.Is(err, ErrSubscriptionExpired) || msg == "订阅已过期" || strings.EqualFold(msg, "subscription expired") {
		return "订阅已过期，请续费后重试"
	}
	if errors.Is(err, ErrNotLoggedIn) {
		if where == InPreferences {
			return "请先登录"
		}
		return "登录已失效，请重新登录"
	}

	var api *syncclient.APIError
	if errors.As(err, &api) {
		return explainAPI(api, where)
	}
	if msg == "missing NAS base url" || errors.Is(err, syncclient.ErrNoBaseURL) {
		return "云同步服务地址缺失，请重启应用后重试"
	}

	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) || strings.Contains(msg, "request timeout"):
		if where == InPreferences {
			return "请求超时，请检查网络和反向代理"
		}
		return "网络请求超时，请检查域名解析、HTTPS 和反向代理"
	case errors.Is(err, context.Canceled):
		if where == InPreferences {
			return "已取消"
		}
		return "请求超时或已取消，请稍后重试"
	case errors.As(err, &netErr):
		if msg != "" {
			return "同步服务不可达：" + msg
		}
		return "同步服务不可达，请稍后重试"
	}

	if where == InPreferences {
		if msg == "" {
			return "同步失败"
		}
		return "同步失败：" + msg
	}
	if msg == "" {
		return "未知错误"
	}
	return msg
}

func explainAPI(api *syncclient.APIError, where Where) string {
	code := strings.TrimSpace(api.Code)
	switch api.StatusCode {
	case 401:
		return "登录已失效，请重新登录"
	case 404:
		return "云端还没有快照"
	case 413:
		return "同步数据过大，请清理后重试"
	case 403:
		switch {
		case isOverLimit(code, api.Body):
			return "云端存储已超限，请清理旧快照或升级套餐"
		case isTierLimited(code, api.Body):
			return "当前套餐不支持该同步操作，请升级套餐后重试"
		case isExpiredText(api.Body):
			return "订阅已过期，请续费后重试"
		case where == InPreferences:
			return "权限不足或订阅状态异常"
		}
	}
	return fmt.Sprintf("同步失败(%d)", api.StatusCode)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func isExpiredText(body string) bool {
	return containsFold(body, "订阅已过期") || (containsFold(body, "subscription") && containsFold(body, "expired"))
}

func isOverLimit(code, body string) bool {
	return containsFold(code, "over_limit") || containsFold(code, "quota") || containsFold(code, "storage") ||
		containsFold(body, "over limit") || containsFold(body, "quota") || containsFold(body, "存储已超限")
}

func isTierLimited(code, body string) bool {
	return containsFold(code, "tier") || containsFold(code, "plan") || containsFold(code, "subscription_required") ||
		containsFold(body, "tier") || containsFold(body, "plan") || containsFold(body, "套餐")
}
