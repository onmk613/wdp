//go:build !unix

package pushcerts

import "io/fs"

// dirOwnerUID 在非 Unix 平台无属主语义（返回 false，跳过属主校验）。
func dirOwnerUID(fs.FileInfo) (uint32, bool) { return 0, false }
