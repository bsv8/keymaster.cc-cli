// Go 静态分析：方法注册、handler 到业务函数的调用路径及可静态确定的内部调用。
// 使用 go/ast 解析；方法字符串经 `go list` 定位 kmapi/methods.gen.go 解析常量得到（类型信息）。
// Daemon 共享直调按真实展示并标归属尚未收口；只有明确依据才计跨插件关系（首版为零）。
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Handler struct {
	Method   string `json:"method"`
	PluginID string `json:"pluginId"`
	UnitID   string `json:"unitId"`
	Handler  string `json:"handler"`
	RegFile  string `json:"regFile"`
	RegLine  int    `json:"regLine"`
	ImplFile string `json:"implFile"`
	ImplLine int    `json:"implLine"`
	Runtime  string `json:"runtime"`
}

type GoCall struct {
	CallerHandler  string `json:"callerHandler"`
	CallerPluginID string `json:"callerPluginId"`
	Callee         string `json:"callee"`
	File           string `json:"file"`
	Line           int    `json:"line"`
	Kind           string `json:"kind"`
	Ownership      string `json:"ownership"`
	Reason         string `json:"reason,omitempty"`
}

type Unresolved struct {
	Side   string `json:"side"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Caller string `json:"caller"`
	Reason string `json:"reason"`
}

type Out struct {
	GoHandlers []Handler    `json:"goHandlers"`
	GoCalls    []GoCall     `json:"goCalls"`
	Unresolved []Unresolved `json:"unresolved"`
}

func repoRel(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

func kmapiDir(root string) string {
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", "github.com/bsv8/KeymasterAPIProtocol/kmapi")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "go list kmapi 失败:", err)
		os.Exit(1)
	}
	return strings.TrimSpace(string(out))
}

func loadMethodMap(root string) map[string]string {
	dir := kmapiDir(root)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(dir, "methods.gen.go"), nil, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析 methods.gen.go 失败:", err)
		os.Exit(1)
	}
	m := map[string]string{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if len(vs.Values) <= i {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					continue
				}
				m[name.Name] = strings.Trim(lit.Value, "\"")
			}
		}
	}
	return m
}

func main() {
	root := os.Args[1]
	daemonDir := filepath.Join(root, "internal/daemon")
	methodMap := loadMethodMap(root)

	type reg struct {
		plugin, method, handler, file string
		line                         int
	}
	var regs []reg
	implPos := map[string]struct {
		file string
		line int
	}{}

	fset := token.NewFileSet()
	// 先收集全部 func (d *Daemon) 定义位置。
	daemonFiles := []string{}
	entries, _ := os.ReadDir(daemonDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			daemonFiles = append(daemonFiles, filepath.Join(daemonDir, e.Name()))
		}
	}
	for _, file := range daemonFiles {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name == nil {
				continue
			}
			pos := fset.Position(fn.Pos())
			implPos[fn.Name.Name] = struct {
				file string
				line int
			}{repoRel(root, file), pos.Line}
		}
	}
	// 解析 plugin_*.go 注册表。
	for _, file := range daemonFiles {
		if !strings.HasPrefix(filepath.Base(file), "plugin_") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			continue
		}
		var plugin string
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "apiPlugin" {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok {
					plugin = strings.Trim(lit.Value, "\"")
				}
				return true
			}
			return true
		})
		// 顶层 apiPlugin 调用内 map 条目：key 为 kmapi.MethodX，value func 内首个 d.opY。
		ast.Inspect(f, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			sel, ok := kv.Key.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok || x.Name != "kmapi" {
				return true
			}
			method := methodMap[sel.Sel.Name]
			if method == "" {
				return true
			}
			handler := ""
			ast.Inspect(kv.Value, func(m ast.Node) bool {
				c, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				s, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if recv, ok := s.X.(*ast.Ident); ok && recv.Name == "d" && strings.HasPrefix(s.Sel.Name, "op") {
					if handler == "" {
						handler = s.Sel.Name
					}
					return false
				}
				return true
			})
			if handler == "" {
				return true
			}
			pos := fset.Position(kv.Pos())
			regs = append(regs, reg{plugin, method, handler, repoRel(root, file), pos.Line})
			return true
		})
	}

	// catalog 提供 plugin->unit。
	catRaw, _ := os.ReadFile(filepath.Join(root, "pluginmanifest/catalog.json"))
	var catalog []struct {
		PluginID string `json:"pluginId"`
		UnitID   string `json:"unitId"`
	}
	_ = json.Unmarshal(catRaw, &catalog)
	unitOf := map[string]string{}
	for _, u := range catalog {
		unitOf[u.PluginID] = u.UnitID
	}

	handlers := []Handler{}
	handlerPlugin := map[string]string{}
	for _, r := range regs {
		impl := implPos[r.handler]
		handlers = append(handlers, Handler{
			Method: r.method, PluginID: r.plugin, UnitID: unitOf[r.plugin],
			Handler: r.handler, RegFile: r.file, RegLine: r.line,
			ImplFile: impl.file, ImplLine: impl.line, Runtime: "go",
		})
		handlerPlugin[r.handler] = r.plugin
	}

	// 分析全部 func (d *Daemon) 体内直接调用。
	var calls []GoCall
	var unresolved []Unresolved
	for _, file := range daemonFiles {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name == nil || fn.Body == nil {
				continue
			}
			caller := fn.Name.Name
			callerPlugin := handlerPlugin[caller]
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pos := fset.Position(c.Pos())
				fileRel := repoRel(root, file)
				switch fun := c.Fun.(type) {
				case *ast.SelectorExpr:
					// d.xxx / d.field.Method
					if recv, ok := fun.X.(*ast.Ident); ok && recv.Name == "d" {
						calls = append(calls, GoCall{
							CallerHandler: caller, CallerPluginID: callerPlugin,
							Callee: "d." + fun.Sel.Name, File: fileRel, Line: pos.Line,
							Kind: "direct", Ownership: "daemon-shared-unconsolidated",
						})
						return true
					}
					if sel, ok := fun.X.(*ast.SelectorExpr); ok {
						if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "d" {
							calls = append(calls, GoCall{
								CallerHandler: caller, CallerPluginID: callerPlugin,
								Callee: "d." + sel.Sel.Name + "." + fun.Sel.Name, File: fileRel, Line: pos.Line,
								Kind: "direct", Ownership: "daemon-shared-unconsolidated",
							})
							return true
						}
					}
					// 其他接收者的方法：目标不可静态确定时列未解析。
					if _, ok := fun.X.(*ast.CallExpr); ok {
						unresolved = append(unresolved, Unresolved{Side: "go", File: fileRel, Line: pos.Line, Caller: caller, Reason: "chained-call-target-unresolved"})
					}
				case *ast.FuncLit:
					// 匿名函数：进入分析但不单独列。
					return true
				case *ast.Ident:
					// 包级函数：记录为直接调用，归属按名判断（非 d.* 不计跨插件）。
					if fun.Name == "go" {
						return true
					}
				case *ast.IndexExpr, *ast.IndexListExpr:
					unresolved = append(unresolved, Unresolved{Side: "go", File: fileRel, Line: pos.Line, Caller: caller, Reason: "generic-or-dynamic-call-unresolved"})
				}
				return true
			})
			// go 语句与 defer 中的动态：简单标记（已在上覆盖，补充 go func 文本）。
			_ = callerPlugin
		}
	}

	sort.Slice(handlers, func(i, j int) bool {
		if handlers[i].Method != handlers[j].Method {
			return handlers[i].Method < handlers[j].Method
		}
		return handlers[i].Handler < handlers[j].Handler
	})
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].CallerHandler != calls[j].CallerHandler {
			return calls[i].CallerHandler < calls[j].CallerHandler
		}
		if calls[i].File != calls[j].File {
			return calls[i].File < calls[j].File
		}
		if calls[i].Line != calls[j].Line {
			return calls[i].Line < calls[j].Line
		}
		return calls[i].Callee < calls[j].Callee
	})
	// 精确去重：同一位置的同一调用只保留一条，避免链式展开重复计数。
	{
		seen := map[string]bool{}
		uniq := calls[:0]
		for _, c := range calls {
			k := c.CallerHandler + "\x00" + c.Callee + "\x00" + c.File + "\x00" + strconv.Itoa(c.Line) + "\x00" + c.Kind
			if seen[k] {
				continue
			}
			seen[k] = true
			uniq = append(uniq, c)
		}
		calls = uniq
	}
	sort.Slice(unresolved, func(i, j int) bool {
		if unresolved[i].File != unresolved[j].File {
			return unresolved[i].File < unresolved[j].File
		}
		if unresolved[i].Line != unresolved[j].Line {
			return unresolved[i].Line < unresolved[j].Line
		}
		return unresolved[i].Reason < unresolved[j].Reason
	})
	{
		seen := map[string]bool{}
		uniq := unresolved[:0]
		for _, u := range unresolved {
			k := u.Side + "\x00" + u.File + "\x00" + strconv.Itoa(u.Line) + "\x00" + u.Caller + "\x00" + u.Reason
			if seen[k] {
				continue
			}
		seen[k] = true
		uniq = append(uniq, u)
		}
		unresolved = uniq
	}
	_ = json.NewEncoder(os.Stdout).Encode(Out{handlers, calls, unresolved})
}
