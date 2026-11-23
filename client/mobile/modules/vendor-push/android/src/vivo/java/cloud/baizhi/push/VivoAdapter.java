package cloud.baizhi.push;
import android.content.Context;
import com.vivo.push.PushClient;
import com.vivo.push.PushConfig;
import com.vivo.push.listener.IPushQueryActionListener;
public final class VivoAdapter implements VendorPushAdapter {
 public void start(Context context) {
try {
 PushClient client=PushClient.getInstance(context);
 client.initialize(new PushConfig.Builder().agreePrivacyStatement(true).build());
 if (!client.isSupport()) {NativePushRegistry.unsupported("vivo_push");return;}
 client.turnOnPush(code -> {
  if (code!=0) { NativePushRegistry.complete("vivo_push");return; }
  client.getRegId(new IPushQueryActionListener() {
   public void onSuccess(String token) {NativePushRegistry.token("vivo_push",token);}
   public void onFail(Integer code) {NativePushRegistry.complete("vivo_push");}
  });
 });
} catch(Exception e) {NativePushRegistry.complete("vivo_push");}
 }
}
