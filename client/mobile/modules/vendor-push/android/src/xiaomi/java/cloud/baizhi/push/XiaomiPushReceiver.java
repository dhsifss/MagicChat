package cloud.baizhi.push;
import android.content.Context;
import com.xiaomi.mipush.sdk.*;
public final class XiaomiPushReceiver extends PushMessageReceiver {
 @Override public void onReceiveRegisterResult(Context context,MiPushCommandMessage message) {
  if(MiPushClient.COMMAND_REGISTER.equals(message.getCommand())&&message.getResultCode()==0&&message.getCommandArguments()!=null&&!message.getCommandArguments().isEmpty()) NativePushRegistry.token("xiaomi_push",message.getCommandArguments().get(0));
  else NativePushRegistry.complete("xiaomi_push");
 }
 @Override public void onNotificationMessageClicked(Context context,MiPushMessage message) { NativePushResponse.fromJSON(message.getContent());NativePushResponse.fromMap(message.getExtra()); }
}
