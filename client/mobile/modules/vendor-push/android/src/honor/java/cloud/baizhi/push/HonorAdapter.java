package cloud.baizhi.push;
import android.content.Context;
import com.hihonor.push.sdk.HonorPushClient;
import com.hihonor.push.sdk.HonorPushCallback;
public final class HonorAdapter implements VendorPushAdapter {
 public void start(Context context) {
HonorPushClient client=HonorPushClient.getInstance();
if (!client.checkSupportHonorPush(context)) { NativePushRegistry.unsupported("honor_push");return; }
client.init(context,false);
client.getPushToken(new HonorPushCallback<String>() {
 public void onSuccess(String token) { NativePushRegistry.token("honor_push",token); }
 public void onFailure(int code,String message) { NativePushRegistry.complete("honor_push"); }
});
 }
}
