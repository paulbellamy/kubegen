package appmaker

import (
	_ "fmt"

	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/pkg/util/intstr"

	"github.com/imdario/mergo"
)

type App struct {
	GroupName               string                     `hcl:"group_name"`
	Components              []AppComponent             `hcl:"-"`
	ComponentsFromImages    []AppComponentFromImage    `hcl:"component_from_image"`
	Templates               []AppComponentTemplate     `hcl:"component_template"`
	ComponentsFromTemplates []AppComponentFromTemplate `hcl:"component_from_template"`
	CommonEnv               map[string]string          `hcl:"common_env"`
}

type AppComponent struct {
	Image     string            `hcl:"-"`
	Name      string            `json:",omitempty" hcl:"name,omitempty"`
	Port      int32             `json:",omitempty" hcl:"port,omitempty"`
	Replicas  *int32            `json:",omitempty" hcl:"replicas,omitempty"`
	Flavor    string            `json:",omitempty" hcl:"flavor,omitempty"`
	Opts      AppComponentOpts  `json:",omitempty" hcl:"opts,omitempty"`
	Env       map[string]string `json:",omitempty" hcl:"env,omitempty"`
	CommonEnv []string          `json:",omitempty" hcl:"common_env,omitempty"`
	// Deployment, DaemonSet, StatefullSet ...etc
	Kind int `json:",omitempty" hcl:"kind,omitempty"`
	// It's probably okay for now, but we'd eventually want to
	// inherit properties defined outside of the AppComponent struct,
	// that it anything we'd use setters and getters for, so we might
	// want to figure out intermediate struct or just write more
	// some tests to see how things would work without that...
	basedOn              *AppComponent        `json:"-" hcl:"-"`
	BasedOnNamedTemplate string               `json:",omitempty" hcl:"based_on,omitempty"`
	Customize            GeneralCustomizer    `json:"-" hcl:"-"`
	CustomizeCotainers   ContainersCustomizer `json:"-" hcl:"-"`
	CustomizePod         PodCustomizer        `json:"-" hcl:"-"`
	CustomizeService     ServiceCustomizer    `json:"-" hcl:"-"`
	CustomizePorts       PortsCustomizer      `json:"-" hcl:"-"`
}

type AppComponentFromImage struct {
	Image        string `hcl:",key"`
	AppComponent `hcl:",squash"`
}

type AppComponentTemplate struct {
	TemplateName string `json:",omitempty" hcl:",key"`
	Image        string `json:",omitempty" hcl:"image"`
	AppComponent `json:",inline" hcl:",squash"`
}

// AppComponentFromTemplate is the same as AppComponentTemplate, but it is an alias, because
// it makes the code easier to read
type AppComponentFromTemplate AppComponentTemplate

// Everything we want to controll per-app, but it's not exposed to HCL directly
type AppParams struct {
	Namespace              string
	DefaultReplicas        int32
	DefaultPort            int32
	StandardLivenessProbe  *v1.Probe
	StandardReadinessProbe *v1.Probe
	templates              map[string]AppComponent
	commonEnv              map[string]string
}

// AppComponentOpts hold highlevel fields which map to a non-trivial settings
// within inside the object, often affecting sub-fields within sub-fields,
// for more trivial things (like hostNetwork) we have custom setters
type AppComponentOpts struct {
	PrometheusPath   string `json:",omitempty"`
	PrometheusScrape bool   `json:",omitempty"`
	// WithoutPorts implies WithoutService and WithoutStandardProbes
	WithoutPorts                   bool   `json:",omitempty"`
	WithoutStandardProbes          bool   `json:",omitempty"`
	WithoutStandardSecurityContext bool   `json:",omitempty"`
	HealthPath                     string `json:",omitempty"`
	LivenessPath                   string `json:",omitempty"`
	// XXX we can add these here, but may be they belong elsewhere?
	//WithProbes interface{}
	//WithSecurityContext interface{}
	// WithoutService disables building of the service
	WithoutService bool `json:",omitempty"`
}

type (
	GeneralCustomizer func(
		*AppComponentResources,
	)
	ContainersCustomizer func(
		[]v1.Container,
	)
	PodCustomizer func(
		*v1.PodSpec,
	)
	ServiceCustomizer func(
		*v1.ServiceSpec,
	)
	PortsCustomizer func(
		servicePorts []v1.ServicePort,
		podPorts ...[]v1.ContainerPort,
	)
)

// Global defaults
const (
	DEFAULT_REPLICAS = int32(1)
	DEFAULT_PORT     = int32(80)
)

const (
	// Deployment is the default kind of general workload, this is what you most likely need to use
	Deployment = iota
	// ReplicaSet is a lower-level kind for a general workload, it's the same as KindDeployment, expcept it doesn't support rolloouts
	ReplicaSet
	// DaemonSet
	DaemonSet
	// StatefullSet
	StatefullSet
	Service
	ConfigMap
	Secret
)

type AppComponentResources struct {
	deployment *v1beta1.Deployment
	service    *v1.Service
	manifest   AppComponent
}

func (i *AppComponent) getNameAndLabels() (string, map[string]string) {
	var name string

	imageParts := strings.Split(strings.Split(i.Image, ":")[0], "/")
	name = imageParts[len(imageParts)-1]

	if i.Name != "" {
		name = i.Name
	}

	labels := map[string]string{"name": name}

	return name, labels
}

func (i *AppComponent) getMeta() metav1.ObjectMeta {
	name, labels := i.getNameAndLabels()
	return metav1.ObjectMeta{
		Name:   name,
		Labels: labels,
	}
}

func (i *AppComponent) getPort(params AppParams) int32 {
	if i.Port != 0 {
		return i.Port
	}
	return params.DefaultPort
}

func (i *AppComponent) maybeAddEnvVars(params AppParams, container *v1.Container) {
	if len(i.Env) == 0 {
		return
	}

	keys := []string{}
	for k, _ := range i.Env {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	env := []v1.EnvVar{}
	for _, j := range keys {
		for k, v := range i.Env {
			if k == j {
				env = append(env, v1.EnvVar{Name: k, Value: v})
			}
		}
	}
	container.Env = env
}

func (i *AppComponent) maybeUseCommonEnvVars(params AppParams) {
	if len(i.CommonEnv) == 0 {
		return
	}

	if i.Env == nil {
		i.Env = make(map[string]string)
	}

	for _, j := range i.CommonEnv {
		if v, ok := params.commonEnv[j]; ok {
			i.Env[j] = v
		}
	}
}

func (i *AppComponent) maybeAddProbes(params AppParams, container *v1.Container) {
	if i.Opts.WithoutStandardProbes {
		return
	}
	port := intstr.FromInt(int(i.getPort(params)))

	container.ReadinessProbe = &v1.Probe{
		PeriodSeconds:       3,
		InitialDelaySeconds: 180,
		Handler: v1.Handler{
			HTTPGet: &v1.HTTPGetAction{
				Path: "/health",
				Port: port,
			},
		},
	}
	container.LivenessProbe = &v1.Probe{
		PeriodSeconds:       3,
		InitialDelaySeconds: 300,
		Handler: v1.Handler{
			HTTPGet: &v1.HTTPGetAction{
				Path: "/health",
				Port: port,
			},
		},
	}
}

func (i *AppComponent) MakeContainer(params AppParams, name string) v1.Container {
	container := v1.Container{Name: name, Image: i.Image}

	i.maybeUseCommonEnvVars(params)
	i.maybeAddEnvVars(params, &container)

	if !i.Opts.WithoutPorts {
		container.Ports = []v1.ContainerPort{{
			Name:          name,
			ContainerPort: i.getPort(params),
		}}
		i.maybeAddProbes(params, &container)
	}

	return container
}

func (i *AppComponent) MakePod(params AppParams) *v1.PodTemplateSpec {
	name, labels := i.getNameAndLabels()
	container := i.MakeContainer(params, name)

	pod := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: labels,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{container},
		},
	}

	return &pod
}

func (i *AppComponent) MakeDeployment(params AppParams, pod *v1.PodTemplateSpec) *v1beta1.Deployment {
	if pod == nil {
		return nil
	}

	meta := i.getMeta()

	replicas := params.DefaultReplicas

	if i.Replicas != nil {
		replicas = *i.Replicas
	}

	deploymentSpec := v1beta1.DeploymentSpec{
		Replicas: &replicas,
		Selector: &metav1.LabelSelector{MatchLabels: meta.Labels},
		Template: *pod,
	}

	deployment := &v1beta1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "extensions/v1beta1",
		},
		ObjectMeta: meta,
		Spec:       deploymentSpec,
	}

	if params.Namespace != "" {
		deployment.ObjectMeta.Namespace = params.Namespace
	}

	return deployment
}

func (i *AppComponent) MakeService(params AppParams) *v1.Service {
	meta := i.getMeta()

	port := v1.ServicePort{Port: i.getPort(params)}
	if i.Port != 0 {
		port.Port = i.Port
	}

	service := &v1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: meta,
		Spec: v1.ServiceSpec{
			Ports:    []v1.ServicePort{port},
			Selector: meta.Labels,
		},
	}

	if params.Namespace != "" {
		service.ObjectMeta.Namespace = params.Namespace
	}

	return service
}

func (i *AppComponent) MakeAll(params AppParams) *AppComponentResources {
	resources := AppComponentResources{}

	if i.BasedOnNamedTemplate != "" {
		if template, ok := params.templates[i.BasedOnNamedTemplate]; ok {
			i.basedOn = &template
		}
	}

	if i.basedOn != nil {
		if i.Env == nil {
			i.Env = make(map[string]string)
		}
		base := *i.basedOn
		if err := mergo.Merge(&base, *i); err != nil {
			panic(err)
		}
		if err := mergo.Merge(i, base); err != nil {
			panic(err)
		}
	}

	resources.manifest = *i

	pod := i.MakePod(params)

	switch i.Kind {
	case Deployment:
		resources.deployment = i.MakeDeployment(params, pod)
	}

	if !i.Opts.WithoutService {
		resources.service = i.MakeService(params)
	}

	if i.Flavor != "" {
		if fn, ok := Flavors[i.Flavor]; ok {
			fn(&resources)
		}
	}

	if i.CustomizePorts != nil && !i.Opts.WithoutPorts {
		ports := make([][]v1.ContainerPort, len(pod.Spec.Containers))
		for n, container := range pod.Spec.Containers {
			ports[n] = container.Ports
		}
		i.CustomizePorts(
			resources.service.Spec.Ports,
			ports...,
		)
	}

	if i.CustomizeCotainers != nil {
		i.CustomizeCotainers(pod.Spec.Containers)
	}

	if i.CustomizePod != nil {
		i.CustomizePod(&pod.Spec)
	}

	if i.CustomizeService != nil {
		i.CustomizeService(&resources.service.Spec)
	}

	if i.Customize != nil {
		i.Customize(&resources)
	}

	return &resources
}

func (i *AppComponent) MakeList(params AppParams) *api.List {
	resources := i.MakeAll(params)

	list := &api.List{}
	switch i.Kind {
	case Deployment:
		list.Items = append(list.Items, runtime.Object(resources.deployment))
	}

	if resources.service != nil {
		list.Items = append(list.Items, runtime.Object(resources.service))

	}

	return list
}

func (i *AppComponentResources) AppendContainer(container v1.Container) AppComponentResources {
	containers := &i.Deployment().Spec.Template.Spec.Containers
	*containers = append(*containers, container)
	return *i
}

func (i *AppComponentResources) MountDataVolume() AppComponentResources {
	// TODO append to volumes and volume mounts based on few simple parameters
	// when user uses more then one container, they will have to do it in a low-level way
	// secrets and config maps would be handled separatelly, so we call this MountDataVolume()
	// and not something else
	return *i
}

func (i *AppComponentResources) WithSecret(secretData interface{}) AppComponentResources {
	return *i
}

func (i *AppComponentResources) WithConfig(configMapData interface{}) AppComponentResources {
	return *i
}

func (i *AppComponentResources) WithExtraLabels(map[string]string) AppComponentResources {
	return *i
}

func (i *AppComponentResources) WithExtraAnnotations(map[string]string) AppComponentResources {
	return *i
}

func (i *AppComponentResources) WithExtraPorts(interface{}) AppComponentResources {
	// TODO May be this should be a customizer, i.e. it'd basically create a PortsCustomizer closure and return it
	return *i
}

func (i *AppComponentResources) UseHostNetwork() AppComponentResources {
	return *i
}

func (i *AppComponentResources) UseHostPID() AppComponentResources {
	return *i
}

func (i *AppComponentResources) Deployment() *v1beta1.Deployment {
	return i.deployment
}

func (i *AppComponentResources) Service() *v1.Service {
	return i.service
}

func (i *AppComponentResources) getPod() *v1.PodSpec {
	switch i.manifest.Kind {
	case Deployment:
		return &i.deployment.Spec.Template.Spec
	default:
		return nil
	}

}

func (i *AppComponentResources) getContainers() []v1.Container {
	switch i.manifest.Kind {
	case Deployment:
		return i.deployment.Spec.Template.Spec.Containers
	default:
		return nil
	}

}

func (i *App) makeDefaultParams() AppParams {
	params := AppParams{
		Namespace:       i.GroupName,
		DefaultReplicas: DEFAULT_REPLICAS,
		DefaultPort:     DEFAULT_PORT,
		templates:       make(map[string]AppComponent),
		// standardSecurityContext
		// standardTmpVolume?
	}

	for _, template := range i.Templates {
		t := &AppComponent{
			Image: template.Image,
			Env:   make(map[string]string),
		}
		if err := mergo.Merge(t, template.AppComponent); err != nil {
			panic(err)
		}
		params.templates[template.TemplateName] = *t
	}

	if len(i.CommonEnv) != 0 {
		params.commonEnv = i.CommonEnv
	}

	return params
}

// TODO: params argument
func (i *App) MakeAll() []*AppComponentResources {
	params := i.makeDefaultParams()

	list := []*AppComponentResources{}
	for _, component := range i.Components {
		list = append(list, component.MakeAll(params))
	}

	for _, component := range i.ComponentsFromImages {
		c := &AppComponent{
			Image: component.Image,
			Env:   make(map[string]string),
		}
		if err := mergo.Merge(c, component.AppComponent); err != nil {
			panic(err)
		}
		list = append(list, c.MakeAll(params))
	}

	for _, component := range i.ComponentsFromTemplates {
		// TODO we may want to return an error if template referenced here is not defined
		c := &AppComponent{
			Image:                component.Image,
			BasedOnNamedTemplate: component.TemplateName,
			Env:                  make(map[string]string),
		}
		if err := mergo.Merge(c, component.AppComponent); err != nil {
			panic(err)
		}
		list = append(list, c.MakeAll(params))
	}

	return list
}

func (i *App) MakeList() *api.List {
	params := i.makeDefaultParams()

	list := &api.List{}
	for _, component := range i.Components {
		list.Items = append(list.Items, component.MakeList(params).Items...)
	}

	for _, component := range i.ComponentsFromImages {
		c := &AppComponent{
			Image: component.Image,
			Env:   make(map[string]string),
		}
		if err := mergo.Merge(c, component.AppComponent); err != nil {
			panic(err)
		}
		list.Items = append(list.Items, c.MakeList(params).Items...)
	}

	for _, component := range i.ComponentsFromTemplates {
		// TODO we may want to return an error if template referenced here is not defined
		c := &AppComponent{
			Image:                component.Image,
			BasedOnNamedTemplate: component.TemplateName,
			Env:                  make(map[string]string),
		}
		if err := mergo.Merge(c, component.AppComponent); err != nil {
			panic(err)
		}
		list.Items = append(list.Items, c.MakeList(params).Items...)
	}

	return list
}
