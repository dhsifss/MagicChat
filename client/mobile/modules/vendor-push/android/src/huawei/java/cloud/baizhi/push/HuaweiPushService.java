package cloud.baizhi.push;
public final class HuaweiPushService extends com.huawei.hms.push.HmsMessageService {
 @Override public void onNewToken(String token) {NativePushRegistry.token("huawei_hms",token);}
}
