package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/grafana/grafana/pkg/apimachinery/identity"
	"github.com/grafana/grafana/pkg/services/user"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/grafana/grafana-app-sdk/logging"
	"github.com/grafana/grafana/pkg/apimachinery/utils"
	grafanarest "github.com/grafana/grafana/pkg/apiserver/rest"
	"github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/apiserver/endpoints/request"
	"github.com/grafana/grafana/pkg/services/live"
	"github.com/grafana/grafana/pkg/services/notifications"
)

// dashboardStorageWrapper is a wrapper around the grafanarest.Storage so it will:
// 1. support adds dashboard permissions handling
// 2. broadcast changes to grafana live
// when running in single tenant mode
type dashboardStorageWrapper struct {
	grafanarest.Storage

	dashboardPermissionsSvc accesscontrol.DashboardPermissionsService
	live                    live.DashboardActivityChannel
	notificationSvc         notifications.Service
	userSvc                 user.Service
}

func (d dashboardStorageWrapper) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	ns, err := request.NamespaceInfoFrom(ctx, true)
	if err != nil {
		return nil, false, err
	}

	obj, created, err := d.Storage.Update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options)
	if err == nil && ns.OrgID > 0 && d.live != nil {
		m, err := utils.MetaAccessor(obj)
		if err == nil {
			if err := d.live.DashboardSaved(ns.Value, name, m.GetResourceVersion()); err != nil {
				logging.FromContext(ctx).Info("live dashboard update failed", "err", err)
			}
		}
	}
	return obj, created, err
}

func (d dashboardStorageWrapper) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	log := logging.FromContext(ctx)

	ns, err := request.NamespaceInfoFrom(ctx, true)
	if err != nil {
		return nil, false, err
	}

	// Get the dashboard definition from the DB
	obj, err := d.Storage.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		log.Warn("dashboard delete failed to get dashboard definition", "err", err)
		return nil, false, err
	}

	// Convert the definition to an array of bytes
	jsonBytes, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		log.Warn("dashboard delete failed to serialize dashboard definition", "err", err)
	}

	// Get the email address of the caller and send the email
	callerEmail, err := d.getCallerEmail(ctx, d.getCaller(ctx))
	if err == nil {
		// Note: we add the email and the serialized definition to the call
		d.sendEmail(ctx, name, callerEmail, jsonBytes)
	}

	// Proceed to delete as before
	obj, async, err := d.Storage.Delete(ctx, name, deleteValidation, options)
	if err != nil {
		return obj, async, err
	}
	if ns.OrgID > 0 && d.live != nil {
		if err := d.live.DashboardDeleted(ns.Value, name); err != nil {
			logging.FromContext(ctx).Info("live dashboard update failed", "err", err)
		}
	}
	if accessErr := d.dashboardPermissionsSvc.DeleteResourcePermissions(ctx, ns.OrgID, name); accessErr != nil {
		return obj, async, accessErr
	}
	return obj, async, nil
}

// Send an email with
// - the name of the dashboard deleted
// - the email of the caller who deleted the dashboard
// - the dashboard definition (JSON string) inlined in the email body (no attachment)
func (d dashboardStorageWrapper) sendEmail(ctx context.Context, name string, callerEmail string, dashboardDefinition []byte) {
	log := logging.FromContext(ctx)
	// Previously we expected an array of recipients, now we just use the caller (string) so convert to array
	callerEmailAsArray := strings.Split(callerEmail, ",")
	title := "Grafana Dashboard"
	to := callerEmailAsArray
	deletedBy := callerEmailAsArray[0] // TODO: use the name instead of the email address
	cmd := &notifications.SendEmailCommand{
		To:       to,
		Template: "dashboard_deleted",
		Data: map[string]any{
			"DashboardTitle": title,
			"DeletedBy":      deletedBy,
			"DeletedAt":      time.Now().Format(time.RFC822),
			"DashboardJSON":  string(dashboardDefinition),
		},
	}
	log.Info(fmt.Sprint(cmd))
	if err := d.notificationSvc.SendEmailCommandHandler(ctx, cmd); err != nil {
		log.Warn("dashboard email on delete: failed to send email", "uid", name, "err", err)
	}
}

// Helper function to get the ID of the caller from the context
func (d dashboardStorageWrapper) getCaller(ctx context.Context) int64 {
	log := logging.FromContext(ctx)

	caller, err := identity.GetRequester(ctx)
	if err != nil {
		log.Warn("dashboard requester: failed to get requester", "err", err)
	}

	userId, err := caller.GetInternalID()
	if err != nil {
		log.Warn("dashboard requester: failed to get internal id", "err", err)
	}

	return userId
}

// Helper function to get the email of the caller given their user ID
func (d dashboardStorageWrapper) getCallerEmail(ctx context.Context, userId int64) (string, error) {
	log := logging.FromContext(ctx)

	u, err := d.userSvc.GetByID(ctx, &user.GetUserByIDQuery{ID: userId})

	if err != nil || u == nil || u.Email == "" {
		log.Warn("dashboard requester: failed to get user email by id", "err", err)
	}

	userEmail := u.Email

	return userEmail, err
}
