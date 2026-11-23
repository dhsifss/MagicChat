package cloud.baizhi.push;
import android.content.Context;
public final class VivoPushReceiver extends com.vivo.push.sdk.OpenClientPushMessageReceiver {
 @Override public void onReceiveRegId(Context context,String token) {NativePushRegistry.token("vivo_push",token);}
 @Override public void onNotificationMessageClicked(Context context,com.vivo.push.model.UPSNotificationMessage message) {NativePushResponse.fromMap(message.getParams());}
}
