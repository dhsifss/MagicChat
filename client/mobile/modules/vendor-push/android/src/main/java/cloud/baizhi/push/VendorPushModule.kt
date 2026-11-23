package cloud.baizhi.push

import android.os.Handler
import android.os.Looper
import expo.modules.kotlin.Promise
import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition

class VendorPushModule : Module() {
  private var lastRefresh = 0L
  private val listener = NativePushRegistry.Listener { sendEvent("onRegistrationChanged", emptyMap<String, String>()) }

  private val responseListener = NativePushResponse.Listener { sendEvent("onNotificationResponse", it) }

  override fun definition() = ModuleDefinition {
    Name("MagicChatVendorPush")
    Events("onRegistrationChanged", "onNotificationResponse")
    Function("getUnsupportedProviders") { NativePushRegistry.unsupportedProviders() }
    Function("getConfiguredProviders") {
      val context = appContext.reactContext
      if (context == null) null else {
        val providers = BuildConfig.PUSH_PROVIDERS.split(',').filter { it.isNotEmpty() }.toMutableList()
        if (context.resources.getIdentifier("gcm_defaultSenderId", "string", context.packageName) != 0) providers.add("fcm")
        providers
      }
    }
    OnCreate {
      appContext.reactContext?.let { NativePushResponse.initialize(it) }
      NativePushRegistry.listener = listener
      NativePushResponse.listener = responseListener
    }
    OnNewIntent { intent -> NativePushResponse.fromIntent(intent) }
    OnActivityEntersForeground { NativePushResponse.fromIntent(appContext.currentActivity?.intent) }
    AsyncFunction("getLastNotificationResponseAsync") {
      NativePushResponse.fromIntent(appContext.currentActivity?.intent)
      NativePushResponse.last()
    }
    Function("clearLastNotificationResponse") { NativePushResponse.clear() }
    OnDestroy {
      if (NativePushRegistry.listener === listener) NativePushRegistry.listener = null
      if (NativePushResponse.listener === responseListener) NativePushResponse.listener = null
    }
    AsyncFunction("getRegistrationsAsync") { promise: Promise ->
      val context = appContext.reactContext?.applicationContext
      if (context == null) {
        promise.reject("push_context_unavailable", "Push context unavailable", null)
      } else {
        Thread {
          try {
            synchronized(this) {
              val now = android.os.SystemClock.elapsedRealtime()
              if (lastRefresh == 0L || now - lastRefresh >= 60_000L) {
                lastRefresh = now
                for (provider in BuildConfig.PUSH_PROVIDERS.split(',').filter { it.isNotEmpty() }) {
                  NativePushRegistry.pending(provider)
                  Handler(Looper.getMainLooper()).post {
                    try {
                      val name = when (provider) {
                        "huawei_hms" -> "HuaweiAdapter"
                        "xiaomi_push" -> "XiaomiAdapter"
                        "oppo_push" -> "OppoAdapter"
                        "vivo_push" -> "VivoAdapter"
                        "honor_push" -> "HonorAdapter"
                        else -> "MeizuAdapter"
                      }
                      val adapter = Class.forName("cloud.baizhi.push.$name").getDeclaredConstructor().newInstance() as VendorPushAdapter
                      adapter.start(context)
                    } catch (_: Exception) { NativePushRegistry.complete(provider) }
                  }
                }
              }
            }
            val deadline = android.os.SystemClock.elapsedRealtime() + 9_000L
            while (NativePushRegistry.isPending() && android.os.SystemClock.elapsedRealtime() < deadline) Thread.sleep(100)
            promise.resolve(NativePushRegistry.snapshot())
          } catch (_: Exception) {
            promise.reject("push_registration_failed", "Push registration unavailable", null)
          }
        }.start()
      }
    }
  }
}
