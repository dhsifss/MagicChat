package cloud.baizhi.push;
import android.content.Context;
import com.meizu.cloud.pushsdk.PushManager;
public final class MeizuAdapter implements VendorPushAdapter {
 public void start(Context context) {
if (!PushManager.isBrandMeizu()) {NativePushRegistry.unsupported("meizu_push");return;}
PushManager.register(context,BuildConfig.MEIZU_APP_ID,BuildConfig.MEIZU_APP_KEY);
String token=PushManager.getPushId(context);
if(token!=null&&!token.isEmpty()) NativePushRegistry.token("meizu_push",token);
 }
}
