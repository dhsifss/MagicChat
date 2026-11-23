-keep class cloud.baizhi.push.** { *; }
-keep class com.xiaomi.mipush.sdk.MiPushMessage {
    public java.lang.String getContent();
}
