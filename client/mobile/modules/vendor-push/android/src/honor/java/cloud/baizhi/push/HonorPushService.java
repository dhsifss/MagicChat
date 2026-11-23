package cloud.baizhi.push;
public final class HonorPushService extends com.hihonor.push.sdk.HonorMessageService {
 @Override public void onNewToken(String token) {NativePushRegistry.token("honor_push",token);}
}
