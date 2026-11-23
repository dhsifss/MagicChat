import * as Application from "expo-application"
import { useEffect, useRef, useState } from "react"
import { Linking, Platform } from "react-native"

import {
  clearPushReminder,
  recordPushReminder,
  setPushReminderExplicitlyDisabled,
  shouldShowPushReminder,
  type PushReminderKind,
} from "@/notifications/push-reminder"
import {
  loadPushReminderState,
  updatePushReminderState,
} from "@/notifications/push-registration-store"
import { useAuth } from "@/providers/auth-provider"
import {
  usePushSynchronizationState,
} from "@/providers/push-coordinator-provider"
import { XGUIDialog } from "@/xgui"

export function PushReminderDialog() {
  const { isAuthenticated, isHydrated, isSigningOut } = useAuth()
  const state = usePushSynchronizationState()
  const previousStateRef = useRef(state)
  const [error, setError] = useState("")
  const [reminder, setReminder] = useState<PushReminderKind | null>(null)
  const appVersion = Application.nativeApplicationVersion?.trim() || "unknown"

  useEffect(() => {
    const previous = previousStateRef.current
    previousStateRef.current = state
    if (state !== "registered" || previous === "registered") return
    setReminder(null)
    setError("")
    void updatePushReminderState((current) =>
      clearPushReminder(
        setPushReminderExplicitlyDisabled(current, false),
        "permission"
      )
    ).catch(() => undefined)
  }, [state])

  useEffect(() => {
    if (
      (Platform.OS !== "android" && Platform.OS !== "ios") ||
      !isHydrated ||
      !isAuthenticated ||
      isSigningOut
    ) {
      return
    }

    const kind = reminderKindForState(state)
    if (!kind || reminder) return
    let cancelled = false

    void loadPushReminderState()
      .then(async (preference) => {
        if (
          cancelled ||
          !shouldShowPushReminder({ appVersion, kind, state: preference })
        ) {
          return
        }
        await updatePushReminderState((current) =>
          recordPushReminder(current, kind, appVersion)
        )
        if (!cancelled) {
          setError("")
          setReminder(kind)
        }
      })
      .catch(() => undefined)

    return () => {
      cancelled = true
    }
  }, [appVersion, isAuthenticated, isHydrated, isSigningOut, reminder, state])

  function close() {
    setError("")
    setReminder(null)
  }

  function openSettings() {
    setError("")
    void Linking.openSettings().catch(() => {
      setError("无法打开系统设置，请手动进入系统设置并允许即应发送通知。")
    })
  }

  return (
    <XGUIDialog
      actions={[
        { label: "暂不提醒", onPress: close },
        { label: "去设置", onPress: openSettings, variant: "primary" },
      ]}
      description={error || "系统通知权限或“消息通知”渠道尚未开启，开启后才能在后台收到新消息提醒。"}
      onOpenChange={(open) => {
        if (!open) close()
      }}
      open={reminder !== null}
      title="开启系统通知"
    />
  )
}

function reminderKindForState(
  state: ReturnType<typeof usePushSynchronizationState>
) {
  if (state === "permission_denied") return "permission" as const
  return null
}
