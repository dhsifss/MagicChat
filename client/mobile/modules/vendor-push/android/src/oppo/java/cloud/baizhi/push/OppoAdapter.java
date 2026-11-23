package cloud.baizhi.push;
import android.content.Context;
import com.heytap.msp.push.HeytapPushManager;
import com.heytap.msp.push.callback.ICallBackResultService;
public final class OppoAdapter implements VendorPushAdapter {
 public void start(Context context) {
HeytapPushManager.init(context,false);
if (!HeytapPushManager.isSupportPush(context)) {NativePushRegistry.unsupported("oppo_push");return;}
HeytapPushManager.register(context,BuildConfig.OPPO_APP_KEY,BuildConfig.OPPO_APP_SECRET,new ICallBackResultService() {
 public void onRegister(int code,String token,String appKey,String appSecret) { if(code==0) NativePushRegistry.token("oppo_push",token); else NativePushRegistry.complete("oppo_push"); }
 public void onUnRegister(int code,String appKey,String appSecret) {}
 public void onSetPushTime(int code,String time) {}
 public void onGetPushStatus(int code,int status) {}
 public void onGetNotificationStatus(int code,int status) {}
 public void onError(int code,String message,String appKey,String appSecret) {NativePushRegistry.complete("oppo_push");}
});
 }
}
