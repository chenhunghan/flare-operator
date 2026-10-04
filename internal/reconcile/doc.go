// Package reconcile holds the helpers every flare-operator controller shares. They operate on
// commonv1alpha1.Managed, so the generic reconciler (internal/generic) and the hand-written
// controllers (Tunnel, VPCService, …) behave the same way.
//
//   - Management policies (policy.go): PoliciesOf(mg) answers CanCreate/CanUpdate/CanDelete/
//     CanLateInitialize. The default ["*"] allows everything; ["Observe"] is read-only and must
//     never write to Cloudflare; any action missing from the list is suppressed.
//   - Deletion (policy.go, finalizer.go): EffectiveDeletionPolicy applies the per-kind default
//     (Descriptor.DefaultDeletionPolicy: Orphan for data-bearing kinds). ShouldDeleteExternal is
//     true only for Delete + a policy that allows Delete. Finalize runs the deletion flow; a
//     multi-step delete returns *WaitError from its deleteExternal step to requeue without an
//     error (DeletionResult). FinalizeAccount is every finalizer's account step: a usable
//     account, a WaitError while it exists but is not usable, or, once the CloudflareAccount is
//     gone, nil after a Warning event ExternalResourceKept (the caller keeps the resource and
//     removes its finalizer).
//   - Conditions (conditions.go): Ready and Synced with the reasons from api/common, stamped with
//     the object's generation; SetObservedGeneration.
//   - External ID (externalid.go): the flare.dev/external-id annotation pins or
//     adopts a Cloudflare resource; PersistExternalID writes it without clobbering status.
//   - Accounts (accounts.go): Resolve(mg) maps spec.accountRef to a Ready CloudflareAccount in
//     the same namespace and returns its cached cfclient.Client and account ID, or an
//     *AccountError (reason AccountNotReady) the caller surfaces with MarkAccountNotReady.
//     It also labels mg flare.dev/account=<accountRef.name> (AccountLabel), which the
//     CloudflareAccount controller uses to block the account's deletion while it is in use, so
//     every managed kind must resolve its account through Resolve. spec.baseURL overrides are
//     refused unless allowed by the BaseURLPolicy (manager flags --allow-base-url-override,
//     --allowed-base-url).
//   - Ownership tags (tags.go): Tagger writes flare.dev/owner=<cluster>/<ns>/<name> through
//     Resource Tagging with GET-merge-PUT (PUT replaces all tags), guarded by If-Match with a
//     bounded retry on 412; an ambiguous 500 read is checked against the tag index before any
//     write. Tagger.Owner reads the owner tag without writing. NoopTagger disables tagging
//     (TaggingEnabled).
//   - Ownership proof (ownership.go): RecordCreated (right after a create; no optimistic lock,
//     UID precondition) and RecordOwnership (after a successful EnsureOwner) write
//     AnnotationOwnershipProof (<uid>/<external ID>); an older build's created-by-uid
//     annotation counts too and is migrated (MigrateLegacyOwnership). MayDeleteExternal decides
//     whether a Delete-policy finalization may delete: only with that record, an owner tag
//     naming the object, or (no tag possible) a pinned external-id annotation. status.id and
//     an ambiguous tags read are never proof; a tag read refused for good keeps the resource;
//     a resource already gone yields neither a delete nor a warning. Every kind must use it
//     before a DELETE.
package reconcile
