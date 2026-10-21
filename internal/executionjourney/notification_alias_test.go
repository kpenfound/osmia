package executionjourney

import "github.com/kpenfound/osmia/internal/service"

var notificationKey = service.TestNotificationKey

const notificationSkipped = service.TestNotificationSkipped
const notificationDropped = service.TestNotificationDropped
const notificationGaveUp = service.TestNotificationGaveUp
const notifyAttempts = service.TestNotifyAttempts

type TailnetListener = service.TailnetListener
