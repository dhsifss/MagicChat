package cloud.baizhi.push;
import android.content.Context;
import com.meizu.cloud.pushsdk.platform.message.RegisterStatus;
public final class MeizuPushReceiver extends com.meizu.cloud.pushsdk.MzPushMessageReceiver {
 @Override public void onRegisterStatus(Context context,RegisterStatus status) {
  if("200".equals(status.getCode())) NativePushRegistry.token("meizu_push",status.getPushId());
  else NativePushRegistry.complete("meizu_push");
 }
 @Override public void onNotificationClicked(Context context,com.meizu.cloud.pushsdk.handler.MzPushMessage message) {NativePushResponse.fromJSON(message.getSelfDefineContentString());}
}
