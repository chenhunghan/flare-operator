package reconcile

import (
	commonv1alpha1 "github.com/chenhunghan/flare-operator/api/common/v1alpha1"
)

// Policies is the effective set of management actions of a managed object.
type Policies struct {
	all bool
	set map[commonv1alpha1.ManagementAction]bool
}

// PoliciesOf returns the effective management policies of mg. An empty list means ["*"].
func PoliciesOf(mg commonv1alpha1.Managed) Policies {
	return ParsePolicies(mg.GetResourceSpec().ManagementPolicies)
}

// ParsePolicies interprets a managementPolicies list. Empty (or containing "*") allows every
// action. Reads (Observe) are always performed: a reconciler cannot act without observing.
func ParsePolicies(list []commonv1alpha1.ManagementAction) Policies {
	p := Policies{set: map[commonv1alpha1.ManagementAction]bool{}}
	if len(list) == 0 {
		p.all = true
	}
	for _, a := range list {
		if a == commonv1alpha1.ManageAll {
			p.all = true
		}
		p.set[a] = true
	}
	return p
}

// Has reports whether action a is allowed.
func (p Policies) Has(a commonv1alpha1.ManagementAction) bool { return p.all || p.set[a] }

// CanCreate reports whether the external resource may be created.
func (p Policies) CanCreate() bool { return p.Has(commonv1alpha1.ManageCreate) }

// CanUpdate reports whether the external resource may be updated.
func (p Policies) CanUpdate() bool { return p.Has(commonv1alpha1.ManageUpdate) }

// CanDelete reports whether the external resource may be deleted.
func (p Policies) CanDelete() bool { return p.Has(commonv1alpha1.ManageDelete) }

// CanLateInitialize reports whether unset spec fields may be filled from the observed state.
func (p Policies) CanLateInitialize() bool { return p.Has(commonv1alpha1.ManageLateInitialize) }

// CanWrite reports whether any Cloudflare write (create, update or delete) is allowed.
// Ownership tags are a write too: callers must not tag when CanWrite is false.
func (p Policies) CanWrite() bool { return p.CanCreate() || p.CanUpdate() || p.CanDelete() }

// ObserveOnly reports whether the object is read-only: no Cloudflare writes and no
// late-initialization of the spec.
func (p Policies) ObserveOnly() bool { return !p.CanWrite() && !p.CanLateInitialize() }

// EffectiveDeletionPolicy returns spec.deletionPolicy, else the kind default, else Delete.
func EffectiveDeletionPolicy(mg commonv1alpha1.Managed, kindDefault commonv1alpha1.DeletionPolicy) commonv1alpha1.DeletionPolicy {
	if p := mg.GetResourceSpec().DeletionPolicy; p != "" {
		return p
	}
	if kindDefault != "" {
		return kindDefault
	}
	return commonv1alpha1.DeletionDelete
}

// ShouldDeleteExternal reports whether deleting mg must delete the Cloudflare resource: the
// effective deletion policy is Delete and the management policies allow Delete.
func ShouldDeleteExternal(mg commonv1alpha1.Managed, kindDefault commonv1alpha1.DeletionPolicy) bool {
	return EffectiveDeletionPolicy(mg, kindDefault) == commonv1alpha1.DeletionDelete && PoliciesOf(mg).CanDelete()
}
