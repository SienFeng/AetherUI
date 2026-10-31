package web

import (
	"os"
	"testing"

	"a-ui/web/service"
)

// 识别表里多写一个 slug、或者图标文件改了名，卡片上就是一个裂图的方块，
// 没有任何一层会报错。
func TestBrandIconsExist(t *testing.T) {
	for _, slug := range service.BrandSlugs() {
		if _, err := os.Stat("assets/brand-icons/" + slug + ".svg"); err != nil {
			t.Errorf("brand %q has no icon: %v", slug, err)
		}
	}
}
