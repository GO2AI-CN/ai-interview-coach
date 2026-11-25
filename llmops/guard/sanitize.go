package guard

import (
	"regexp"
	"strings"
)

var linkRe = regexp.MustCompile(`https?://[^\s"']+`)

// SanitizeOutput 输出消毒（第 7 篇第 4 层防线）：非白名单域名脱敏，
// 防止注入塞进来的钓鱼链接被原样展示或转发
func SanitizeOutput(text string) string {
	return linkRe.ReplaceAllStringFunc(text, func(u string) string {
		if strings.Contains(u, "your-company.com") || strings.Contains(u, "911015.com") {
			return u
		}
		return "[已屏蔽的外部链接]"
	})
}
