package genapp

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/goadesign/goa/design"
	"github.com/goadesign/goa/goagen/codegen"
	"github.com/goadesign/goa/goagen/utils"
)

// Generator is the application code generator.
type Generator struct {
	outDir   string   // Path to output directory
	target   string   // Name of generated package
	notest   bool     // Whether to skip test generation
	genfiles []string // Generated files
}

// Generate is the generator entry point called by the meta generator.
func Generate() (files []string, err error) {
	var (
		outDir, target string
		notest         bool
	)

	set := flag.NewFlagSet("app", flag.PanicOnError)
	set.String("design", "", "")
	set.StringVar(&outDir, "out", "", "")
	set.StringVar(&target, "pkg", "app", "")
	set.BoolVar(&notest, "notest", false, "")
	set.Parse(os.Args[2:])
	outDir = filepath.Join(outDir, target)

	target = codegen.Goify(target, false)
	g := &Generator{outDir: outDir, target: target, notest: notest}
	codegen.Reserved[target] = true

	return g.Generate(design.Design)
}

// Generate the application code, implement codegen.Generator.
func (g *Generator) Generate(api *design.APIDefinition) (_ []string, err error) {
	if api == nil {
		return nil, fmt.Errorf("missing API definition, make sure design is properly initialized")
	}

	go utils.Catch(nil, func() { g.Cleanup() })

	defer func() {
		if err != nil {
			g.Cleanup()
		}
	}()

	os.RemoveAll(g.outDir)

	if err := os.MkdirAll(g.outDir, 0755); err != nil {
		return nil, err
	}
	g.genfiles = []string{g.outDir}
	if err := g.generateContexts(api); err != nil {
		return nil, err
	}
	if err := g.generateControllers(api); err != nil {
		return nil, err
	}
	if err := g.generateSecurity(api); err != nil {
		return nil, err
	}
	if err := g.generateHrefs(api); err != nil {
		return nil, err
	}
	if err := g.generateMediaTypes(api); err != nil {
		return nil, err
	}
	if err := g.generateUserTypes(api); err != nil {
		return nil, err
	}
	if !g.notest {
		if err := g.generateResourceTest(api); err != nil {
			return nil, err
		}
	}

	return g.genfiles, nil
}

// Cleanup removes the entire "app" directory if it was created by this generator.
func (g *Generator) Cleanup() {
	if len(g.genfiles) == 0 {
		return
	}
	os.RemoveAll(g.outDir)
	g.genfiles = nil
}

// generateContexts iterates through the API resources and actions and generates the action
// contexts.
func (g *Generator) generateContexts(api *design.APIDefinition) error {
	ctxFile := filepath.Join(g.outDir, "contexts.go")
	ctxWr, err := NewContextsWriter(ctxFile)
	if err != nil {
		panic(err) // bug
	}
	title := fmt.Sprintf("%s: Application Contexts", api.Context())
	imports := []*codegen.ImportSpec{
		codegen.SimpleImport("fmt"),
		codegen.SimpleImport("golang.org/x/net/context"),
		codegen.SimpleImport("strconv"),
		codegen.SimpleImport("strings"),
		codegen.SimpleImport("time"),
		codegen.SimpleImport("github.com/goadesign/goa"),
		codegen.NewImport("uuid", "github.com/satori/go.uuid"),
	}
	g.genfiles = append(g.genfiles, ctxFile)
	ctxWr.WriteHeader(title, g.target, imports)
	err = api.IterateResources(func(r *design.ResourceDefinition) error {
		return r.IterateActions(func(a *design.ActionDefinition) error {
			ctxName := codegen.Goify(a.Name, true) + codegen.Goify(a.Parent.Name, true) + "Context"
			headers := r.Headers.Merge(a.Headers)
			if headers != nil && len(headers.Type.ToObject()) == 0 {
				headers = nil // So that {{if .Headers}} returns false in templates
			}
			params := a.AllParams()
			if params != nil && len(params.Type.ToObject()) == 0 {
				params = nil // So that {{if .Params}} returns false in templates
			}

			non101 := make(map[string]*design.ResponseDefinition)
			for k, v := range a.Responses {
				if v.Status != 101 {
					non101[k] = v
				}
			}
			ctxData := ContextTemplateData{
				Name:         ctxName,
				ResourceName: r.Name,
				ActionName:   a.Name,
				Payload:      a.Payload,
				Params:       params,
				Headers:      headers,
				Routes:       a.Routes,
				Responses:    non101,
				API:          api,
				DefaultPkg:   g.target,
				Security:     a.Security,
			}
			return ctxWr.Execute(&ctxData)
		})
	})
	if err != nil {
		return err
	}
	return ctxWr.FormatCode()
}

// generateControllers iterates through the API resources and generates the low level
// controllers.
func (g *Generator) generateControllers(api *design.APIDefinition) error {
	ctlFile := filepath.Join(g.outDir, "controllers.go")
	ctlWr, err := NewControllersWriter(ctlFile)
	if err != nil {
		panic(err) // bug
	}
	title := fmt.Sprintf("%s: Application Controllers", api.Context())
	imports := []*codegen.ImportSpec{
		codegen.SimpleImport("net/http"),
		codegen.SimpleImport("fmt"),
		codegen.SimpleImport("golang.org/x/net/context"),
		codegen.SimpleImport("github.com/goadesign/goa"),
		codegen.SimpleImport("github.com/goadesign/goa/cors"),
	}
	encoders, err := BuildEncoders(api.Produces, true)
	if err != nil {
		return err
	}
	decoders, err := BuildEncoders(api.Consumes, false)
	if err != nil {
		return err
	}
	encoderImports := make(map[string]bool)
	for _, data := range encoders {
		encoderImports[data.PackagePath] = true
	}
	for _, data := range decoders {
		encoderImports[data.PackagePath] = true
	}
	var packagePaths []string
	for packagePath := range encoderImports {
		if packagePath != "github.com/goadesign/goa" {
			packagePaths = append(packagePaths, packagePath)
		}
	}
	sort.Strings(packagePaths)
	for _, packagePath := range packagePaths {
		imports = append(imports, codegen.SimpleImport(packagePath))
	}
	ctlWr.WriteHeader(title, g.target, imports)
	ctlWr.WriteInitService(encoders, decoders)

	var controllersData []*ControllerTemplateData
	err = api.IterateResources(func(r *design.ResourceDefinition) error {
		data := &ControllerTemplateData{
			API:            api,
			Resource:       codegen.Goify(r.Name, true),
			PreflightPaths: r.PreflightPaths(),
			FileServers:    r.FileServers,
		}
		ierr := r.IterateActions(func(a *design.ActionDefinition) error {
			context := fmt.Sprintf("%s%sContext", codegen.Goify(a.Name, true), codegen.Goify(r.Name, true))
			unmarshal := fmt.Sprintf("unmarshal%s%sPayload", codegen.Goify(a.Name, true), codegen.Goify(r.Name, true))
			action := map[string]interface{}{
				"Name":            codegen.Goify(a.Name, true),
				"Routes":          a.Routes,
				"Context":         context,
				"Unmarshal":       unmarshal,
				"Payload":         a.Payload,
				"PayloadOptional": a.PayloadOptional,
				"Security":        a.Security,
			}
			data.Actions = append(data.Actions, action)
			return nil
		})
		if ierr != nil {
			return ierr
		}
		if len(data.Actions) > 0 || len(data.FileServers) > 0 {
			data.Encoders = encoders
			data.Decoders = decoders
			data.Origins = r.AllOrigins()
			controllersData = append(controllersData, data)
		}
		return nil
	})
	if err != nil {
		return err
	}
	g.genfiles = append(g.genfiles, ctlFile)
	if err = ctlWr.Execute(controllersData); err != nil {
		return err
	}
	return ctlWr.FormatCode()
}

// generateControllers iterates through the API resources and generates the low level
// controllers.
func (g *Generator) generateSecurity(api *design.APIDefinition) error {
	if len(api.SecuritySchemes) == 0 {
		return nil
	}

	secFile := filepath.Join(g.outDir, "security.go")
	secWr, err := NewSecurityWriter(secFile)
	if err != nil {
		panic(err) // bug
	}

	title := fmt.Sprintf("%s: Application Security", api.Context())
	imports := []*codegen.ImportSpec{
		codegen.SimpleImport("net/http"),
		codegen.SimpleImport("errors"),
		codegen.SimpleImport("golang.org/x/net/context"),
		codegen.SimpleImport("github.com/goadesign/goa"),
	}
	secWr.WriteHeader(title, g.target, imports)

	g.genfiles = append(g.genfiles, secFile)

	if err = secWr.Execute(design.Design.SecuritySchemes); err != nil {
		return err
	}

	return secWr.FormatCode()
}

// generateHrefs iterates through the API resources and generates the href factory methods.
func (g *Generator) generateHrefs(api *design.APIDefinition) error {
	hrefFile := filepath.Join(g.outDir, "hrefs.go")
	resWr, err := NewResourcesWriter(hrefFile)
	if err != nil {
		panic(err) // bug
	}
	title := fmt.Sprintf("%s: Application Resource Href Factories", api.Context())
	imports := []*codegen.ImportSpec{
		codegen.SimpleImport("fmt"),
	}
	resWr.WriteHeader(title, g.target, imports)
	err = api.IterateResources(func(r *design.ResourceDefinition) error {
		m := api.MediaTypeWithIdentifier(r.MediaType)
		var identifier string
		if m != nil {
			identifier = m.Identifier
		} else {
			identifier = "text/plain"
		}
		data := ResourceData{
			Name:              codegen.Goify(r.Name, true),
			Identifier:        identifier,
			Description:       r.Description,
			Type:              m,
			CanonicalTemplate: codegen.CanonicalTemplate(r),
			CanonicalParams:   codegen.CanonicalParams(r),
		}
		return resWr.Execute(&data)
	})
	g.genfiles = append(g.genfiles, hrefFile)
	if err != nil {
		return err
	}
	return resWr.FormatCode()
}

// generateMediaTypes iterates through the media types and generate the data structures and
// marshaling code.
func (g *Generator) generateMediaTypes(api *design.APIDefinition) error {
	mtFile := filepath.Join(g.outDir, "media_types.go")
	mtWr, err := NewMediaTypesWriter(mtFile)
	if err != nil {
		panic(err) // bug
	}
	title := fmt.Sprintf("%s: Application Media Types", api.Context())
	imports := []*codegen.ImportSpec{
		codegen.SimpleImport("github.com/goadesign/goa"),
		codegen.SimpleImport("fmt"),
		codegen.SimpleImport("time"),
		codegen.NewImport("uuid", "github.com/satori/go.uuid"),
	}
	mtWr.WriteHeader(title, g.target, imports)
	err = api.IterateMediaTypes(func(mt *design.MediaTypeDefinition) error {
		if mt.IsBuiltIn() {
			return nil
		}
		if mt.Type.IsObject() || mt.Type.IsArray() {
			return mtWr.Execute(mt)
		}
		return nil
	})
	g.genfiles = append(g.genfiles, mtFile)
	if err != nil {
		return err
	}
	return mtWr.FormatCode()
}

// generateUserTypes iterates through the user types and generates the data structures and
// marshaling code.
func (g *Generator) generateUserTypes(api *design.APIDefinition) error {
	utFile := filepath.Join(g.outDir, "user_types.go")
	utWr, err := NewUserTypesWriter(utFile)
	if err != nil {
		panic(err) // bug
	}
	title := fmt.Sprintf("%s: Application User Types", api.Context())
	imports := []*codegen.ImportSpec{
		codegen.SimpleImport("github.com/goadesign/goa"),
		codegen.SimpleImport("fmt"),
		codegen.SimpleImport("time"),
	}
	utWr.WriteHeader(title, g.target, imports)
	err = api.IterateUserTypes(func(t *design.UserTypeDefinition) error {
		return utWr.Execute(t)
	})
	g.genfiles = append(g.genfiles, utFile)
	if err != nil {
		return err
	}
	return utWr.FormatCode()
}
