package cloud.baizhi.push;
import android.content.Context;
import com.xiaomi.mipush.sdk.MiPushClient;
public final class XiaomiAdapter implements VendorPushAdapter {
 public void start(Context context) {
MiPushClient.registerPush(context,BuildConfig.XIAOMI_APP_ID,BuildConfig.XIAOMI_APP_KEY);
String token=MiPushClient.getRegId(context);
if (token!=null&&!token.isEmpty()) NativePushRegistry.token("xiaomi_push",token);
 }
}
