package generate

import (
	"fmt"

	"github.com/golang/glog"
	kyverno "github.com/nirmata/kyverno/pkg/api/kyverno/v1"
	dclient "github.com/nirmata/kyverno/pkg/dclient"
	"github.com/nirmata/kyverno/pkg/engine"
	"github.com/nirmata/kyverno/pkg/engine/context"
	"github.com/nirmata/kyverno/pkg/engine/validate"
	"github.com/nirmata/kyverno/pkg/engine/variables"
	"github.com/nirmata/kyverno/pkg/policyviolation"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func (c *Controller) processGR(gr *kyverno.GenerateRequest) error {
	// 1 - Check if the resource exists
	resource, err := getResource(c.client, gr.Spec.Resource)
	if err != nil {
		// Dont update status
		glog.V(4).Infof("resource does not exist or is yet to be created, requeuing: %v", err)
		return err
	}

	glog.V(4).Infof("processGR %v", gr.Status.State)
	// 2 - Apply the generate policy on the resource
	err = c.applyGenerate(*resource, *gr)
	switch e := err.(type) {
	case *Violation:
		// Generate event
		// - resource -> rule failed and created PV
		// - policy -> failed to apply of resource and created PV
		c.pvGenerator.Add(generatePV(*gr, *resource, e))
	default:
		// Generate event
		// - resource -> rule failed
		// - policy -> failed tp apply on resource
		glog.V(4).Info(e)
	}
	// 3 - Report Events
	reportEvents(err, c.eventGen, *gr, *resource)

	// 4 - Update Status
	return updateStatus(c.statusControl, *gr, err)
}

func (c *Controller) applyGenerate(resource unstructured.Unstructured, gr kyverno.GenerateRequest) error {
	// Get the list of rules to be applied
	// get policy
	glog.V(4).Info("applyGenerate")
	policy, err := c.pLister.Get(gr.Spec.Policy)
	if err != nil {
		glog.V(4).Infof("policy %s not found: %v", gr.Spec.Policy, err)
		return nil
	}
	// build context
	ctx := context.NewContext()
	resourceRaw, err := resource.MarshalJSON()
	if err != nil {
		glog.V(4).Infof("failed to marshal resource: %v", err)
		return err
	}

	ctx.AddResource(resourceRaw)
	ctx.AddUserInfo(gr.Spec.Context.UserRequestInfo)

	policyContext := engine.PolicyContext{
		NewResource:   resource,
		Policy:        *policy,
		Context:       ctx,
		AdmissionInfo: gr.Spec.Context.UserRequestInfo,
	}

	glog.V(4).Info("GenerateNew")
	// check if the policy still applies to the resource
	engineResponse := engine.GenerateNew(policyContext)
	if len(engineResponse.PolicyResponse.Rules) == 0 {
		glog.V(4).Infof("policy %s, dont not apply to resource %v", gr.Spec.Policy, gr.Spec.Resource)
		return fmt.Errorf("policy %s, dont not apply to resource %v", gr.Spec.Policy, gr.Spec.Resource)
	}
	glog.V(4).Infof("%v", gr)
	// Apply the generate rule on resource
	return applyGeneratePolicy(c.client, policyContext, gr.Status.State)
}

func updateStatus(statusControl StatusControlInterface, gr kyverno.GenerateRequest, err error) error {
	if err != nil {
		return statusControl.Failed(gr, err.Error())
	}

	// Generate request successfully processed
	return statusControl.Success(gr)
}

func applyGeneratePolicy(client *dclient.Client, policyContext engine.PolicyContext, state kyverno.GenerateRequestState) error {
	// Get the response as the actions to be performed on the resource
	// - DATA (rule.Generation.Data)
	// - - substitute values
	policy := policyContext.Policy
	resource := policyContext.NewResource
	ctx := policyContext.Context
	glog.V(4).Info("applyGeneratePolicy")
	// To manage existing resources, we compare the creation time for the default resiruce to be generated and policy creation time
	processExisting := func() bool {
		rcreationTime := resource.GetCreationTimestamp()
		pcreationTime := policy.GetCreationTimestamp()
		return rcreationTime.Before(&pcreationTime)
	}()

	for _, rule := range policy.Spec.Rules {
		if !rule.HasGenerate() {
			continue
		}
		if err := applyRule(client, rule, resource, ctx, state, processExisting); err != nil {
			return err
		}
	}

	return nil
}

func applyRule(client *dclient.Client, rule kyverno.Rule, resource unstructured.Unstructured, ctx context.EvalInterface, state kyverno.GenerateRequestState, processExisting bool) error {
	var rdata map[string]interface{}
	var err error

	// variable substitution
	// - name
	// - namespace
	// - clone.name
	// - clone.namespace
	gen := variableSubsitutionForAttributes(rule.Generation, ctx)
	// DATA
	glog.V(4).Info("applyRule")
	if gen.Data != nil {
		if rdata, err = handleData(rule.Name, gen, client, resource, ctx, state); err != nil {
			glog.V(4).Info(err)
			switch e := err.(type) {
			case *ParseFailed, *NotFound, *ConfigNotFound:
				// handled errors
			case *Violation:
				// create policy violation
				return e
			default:
				// errors that cant be handled
				return e
			}
		}
		if rdata == nil {
			// existing resource contains the configuration
			return nil
		}
	}
	// CLONE
	if gen.Clone != (kyverno.CloneFrom{}) {
		if rdata, err = handleClone(gen, client, resource, ctx, state); err != nil {
			switch e := err.(type) {
			case *NotFound:
				// handled errors
				return e
			default:
				// errors that cant be handled
				return e
			}
		}
		if rdata == nil {
			// resource already exists
			return nil
		}
	}
	if processExisting {
		// handle existing resources
		// policy was generated after the resource
		// we do not create new resource
		return err
	}
	// Create the generate resource
	newResource := &unstructured.Unstructured{}
	glog.V(4).Info(rdata)
	newResource.SetUnstructuredContent(rdata)
	newResource.SetName(gen.Name)
	newResource.SetNamespace(gen.Namespace)
	// Reset resource version
	newResource.SetResourceVersion("")
	// set the ownerReferences
	ownerRefs := newResource.GetOwnerReferences()
	// add ownerRefs
	newResource.SetOwnerReferences(ownerRefs)

	glog.V(4).Infof("creating resource %v", newResource)
	_, err = client.CreateResource(gen.Kind, gen.Namespace, newResource, false)
	if err != nil {
		glog.Info(err)
		return err
	}
	glog.V(4).Infof("created new resource %s %s %s ", gen.Kind, gen.Namespace, gen.Name)
	// New Resource created succesfully
	return nil
}

func variableSubsitutionForAttributes(gen kyverno.Generation, ctx context.EvalInterface) kyverno.Generation {
	// Name
	name := gen.Name
	namespace := gen.Namespace
	newNameVar := variables.SubstituteVariables(ctx, name)

	if newName, ok := newNameVar.(string); ok {
		gen.Name = newName
	}

	newNamespaceVar := variables.SubstituteVariables(ctx, namespace)
	if newNamespace, ok := newNamespaceVar.(string); ok {
		gen.Namespace = newNamespace
	}
	// Clone
	cloneName := gen.Clone.Name
	cloneNamespace := gen.Clone.Namespace

	newcloneNameVar := variables.SubstituteVariables(ctx, cloneName)
	if newcloneName, ok := newcloneNameVar.(string); ok {
		gen.Clone.Name = newcloneName
	}
	newcloneNamespaceVar := variables.SubstituteVariables(ctx, cloneNamespace)
	if newcloneNamespace, ok := newcloneNamespaceVar.(string); ok {
		gen.Clone.Namespace = newcloneNamespace
	}
	glog.V(4).Infof("var updated %v", gen.Name)
	return gen
}

func createOwnerReference(ownerRefs []metav1.OwnerReference, resource unstructured.Unstructured) {
	controllerFlag := true
	blockOwnerDeletionFlag := true
	ownerRef := metav1.OwnerReference{
		APIVersion:         resource.GetAPIVersion(),
		Kind:               resource.GetKind(),
		Name:               resource.GetName(),
		UID:                resource.GetUID(),
		Controller:         &controllerFlag,
		BlockOwnerDeletion: &blockOwnerDeletionFlag,
	}
	ownerRefs = append(ownerRefs, ownerRef)
}

func handleData(ruleName string, generateRule kyverno.Generation, client *dclient.Client, resource unstructured.Unstructured, ctx context.EvalInterface, state kyverno.GenerateRequestState) (map[string]interface{}, error) {
	newData := variables.SubstituteVariables(ctx, generateRule.Data)

	// check if resource exists
	obj, err := client.GetResource(generateRule.Kind, generateRule.Namespace, generateRule.Name)
	glog.V(4).Info(err)
	if errors.IsNotFound(err) {
		glog.V(4).Info("handleData NotFound")
		glog.V(4).Info(string(state))
		// Resource does not exist
		if state == "" {
			// Processing the request first time
			rdata, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&newData)
			glog.V(4).Info(err)
			if err != nil {
				return nil, NewParseFailed(newData, err)
			}
			return rdata, nil
		}
		glog.V(4).Info("Creating violation")
		// State : Failed,Completed
		// request has been processed before, so dont create the resource
		// report Violation to notify the error
		return nil, NewViolation(ruleName, NewNotFound(generateRule.Kind, generateRule.Namespace, generateRule.Name))
	}
	glog.V(4).Info(err)
	if err != nil {
		//something wrong while fetching resource
		return nil, err
	}
	// Resource exists; verfiy the content of the resource
	ok, err := checkResource(ctx, newData, obj)
	if err != nil {
		//something wrong with configuration
		glog.V(4).Info(err)
		return nil, err
	}
	if !ok {
		return nil, NewConfigNotFound(newData, generateRule.Kind, generateRule.Namespace, generateRule.Name)
	}
	// Existing resource does contain the required
	return nil, nil
}

func handleClone(generateRule kyverno.Generation, client *dclient.Client, resource unstructured.Unstructured, ctx context.EvalInterface, state kyverno.GenerateRequestState) (map[string]interface{}, error) {
	// check if resource exists
	_, err := client.GetResource(generateRule.Kind, generateRule.Namespace, generateRule.Name)
	if err == nil {
		glog.V(4).Info("handleClone Exists")
		// resource exists
		return nil, nil
	}
	if !errors.IsNotFound(err) {
		glog.V(4).Info("handleClone NotFound")
		//something wrong while fetching resource
		return nil, err
	}

	// get reference clone resource
	obj, err := client.GetResource(generateRule.Kind, generateRule.Clone.Namespace, generateRule.Clone.Name)
	if errors.IsNotFound(err) {
		glog.V(4).Info("handleClone reference not Found")
		return nil, NewNotFound(generateRule.Kind, generateRule.Clone.Namespace, generateRule.Clone.Name)
	}
	if err != nil {
		glog.V(4).Info("handleClone reference Error")
		//something wrong while fetching resource
		return nil, err
	}
	glog.V(4).Info("handleClone refrerence sending")
	return obj.UnstructuredContent(), nil
}

func checkResource(ctx context.EvalInterface, newResourceSpec interface{}, resource *unstructured.Unstructured) (bool, error) {
	// check if the resource spec if a subset of the resource
	path, err := validate.ValidateResourceWithPattern(ctx, resource.Object, newResourceSpec)
	if err != nil {
		glog.V(4).Infof("config not a subset of resource. failed at path %s: %v", path, err)
		return false, err
	}
	return true, nil
}

func generatePV(gr kyverno.GenerateRequest, resource unstructured.Unstructured, err *Violation) policyviolation.Info {

	info := policyviolation.Info{
		Blocked:    false,
		PolicyName: gr.Spec.Policy,
		Resource:   resource,
		Rules: []kyverno.ViolatedRule{kyverno.ViolatedRule{
			Name:    err.rule,
			Type:    "Generation",
			Message: err.Error(),
		}},
	}
	return info
}
