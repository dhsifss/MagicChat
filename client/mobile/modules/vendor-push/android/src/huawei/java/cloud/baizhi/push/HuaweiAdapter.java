package cloud.baizhi.push;
import android.content.Context;
import com.huawei.hms.aaid.HmsInstanceId;
import com.huawei.hms.push.HmsMessaging;
import com.huawei.agconnect.config.AGConnectServicesConfig;
public final class HuaweiAdapter implements VendorPushAdapter {
 public void start(Context context) {
new Thread(() -> {
 try (java.io.InputStream config=context.getAssets().open("magicchat-agconnect-services.json")) {
  AGConnectServicesConfig.fromContext(context).overlayWith(config);
  HmsMessaging.getInstance(context).setAutoInitEnabled(false);
  NativePushRegistry.token("huawei_hms",HmsInstanceId.getInstance(context).getToken(BuildConfig.HUAWEI_APP_ID,"HCM"));
 } catch (Exception e) { NativePushRegistry.complete("huawei_hms"); }
}).start();
 }
}
