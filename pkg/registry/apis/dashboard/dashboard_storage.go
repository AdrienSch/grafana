package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	ns, err := request.NamespaceInfoFrom(ctx, true)
	if err != nil {
		return nil, false, err
	}

	// this is where we need to send an email
	recipients := "user1@example.com"
	if d.dashboardPermissionsSvc != nil {
		d.sendEmail(ctx, name, recipients)
	}

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

func (d dashboardStorageWrapper) sendEmail(ctx context.Context, name string, recipients string) {
	log := logging.FromContext(ctx)
	title := "Grafana Dashboard"
	to := strings.Split(recipients, ",")
	deletedBy := "admin@example.com"
	jsonBytes := "{\"id\":\"123\",\"status\":\"deleted\"}"
	//replyTo := []string{"admin@grfana.org"}
	cmd := &notifications.SendEmailCommand{
		To:       to,
		Template: "dashboard_deleted",
		//Subject:  "Dashboard Deleted",
		//ReplyTo:  replyTo,

		Data: map[string]any{
			"DashboardTitle": title,
			"DeletedBy":      deletedBy,
			"DeletedAt":      time.Now().Format(time.RFC822),
			//"DashboardJSON":  string(jsonBytes),
			"DashboardJSON": jsonBytes,
		},
	}
	log.Info(fmt.Sprint(cmd))
	if err := d.notificationSvc.SendEmailCommandHandler(ctx, cmd); err != nil {
		log.Warn("dashboard email on delete: failed to send email", "uid", name, "err", err)
	}
}
