package cdn

import (
	"slices"
	"strings"
)

// operations are the URL API operation names ucfs accepts after "-/". The
// list is the union of two references, regenerated with:
//
//	rg -o -- '`-/[a-z_0-9]+/' uploadcare-api-references/specs/url-api/index.yaml | sort -u
//	rg -o 'Name: +"[a-z_]+"' uploadcare-cli/internal/cmd/url_api_schema.go | sort -u
//
// The first is the operation tables in the tag descriptions of the URL API
// spec (uploadcare-api-references cfc3fc2), plus json, which the spec has as
// the path /{uuid}/-/json/. The second is buildOperations in uploadcare-cli
// (2380d03) and adds nothing to the first. gif2video and detect_faces follow
// the uuid without "-/", so they are not operations here.
var operations = map[string]bool{
	"autorotate": true, "blur": true, "blur_region": true, "border_radius": true, "brightness": true,
	"contrast": true, "crop": true, "enhance": true, "exposure": true, "filter": true, "flip": true,
	"format": true, "gamma": true, "grayscale": true, "inline": true, "invert": true, "json": true,
	"main_colors": true, "max_icc_size": true, "mirror": true, "overlay": true, "preview": true,
	"progressive": true, "quality": true, "rasterize": true, "rect": true, "resize": true, "rotate": true,
	"saturation": true, "scale_crop": true, "setfill": true, "sharp": true, "smart_resize": true,
	"srgb": true, "strip_meta": true, "text": true, "trim": true, "vibrance": true, "warmth": true,
	"zoom_objects": true,
}

// IsOperation reports whether name is a known operation. Arguments are not
// checked: the CDN judges them.
func IsOperation(name string) bool { return operations[name] }

// OutputExt is the file extension of what the CDN returns for an operations
// path such as "-/resize/1200x/-/format/webp/", or "" when the output keeps
// the original's format. The last operation that sets a format wins.
func OutputExt(ops string) string {
	ext := ""
	for _, op := range splitOps(ops) {
		switch {
		case op[0] == "format" && len(op) > 1:
			switch op[1] {
			case "auto", "preserve":
				ext = ""
			case "jpeg":
				ext = "jpg"
			default:
				ext = op[1]
			}
		case op[0] == "json" || op[0] == "main_colors":
			ext = "json"
		}
	}
	return ext
}

// splitOps splits "-/a/x/-/b/" into [[a x] [b]]; an empty operation after a
// trailing "-/" is left out.
func splitOps(ops string) [][]string {
	var out [][]string
	for _, c := range strings.Split(strings.TrimSuffix(ops, "/"), "/") {
		switch {
		case c == "-":
			out = append(out, nil)
		case len(out) > 0:
			out[len(out)-1] = append(out[len(out)-1], c)
		}
	}
	return slices.DeleteFunc(out, func(op []string) bool { return len(op) == 0 })
}
