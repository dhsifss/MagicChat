package cloud.baizhi.push;

import android.content.Intent;
import android.content.Context;
import android.content.SharedPreferences;
import android.os.Bundle;
import android.text.TextUtils;
import org.json.JSONObject;
import java.util.*;

/** Extract only opaque MagicChat routes. Authentication is enforced when JS resolves them. */
public final class NativePushResponse {
 public interface Listener { void response(Map<String,Object> response); }
 public static volatile Listener listener;
 private static Map<String,Object> last;
 private static SharedPreferences preferences;
 private static final Set<String> consumed = new LinkedHashSet<>();
 public static synchronized void initialize(Context context) {
  if(preferences!=null)return;
  preferences=context.getApplicationContext().getSharedPreferences("magicchat_push_responses",Context.MODE_PRIVATE);
  String stored=preferences.getString("consumed","");
  if(stored!=null&&!stored.isEmpty())Collections.addAll(consumed,stored.split("\n"));
  if(last!=null&&consumed.contains(last.get("identifier")))last=null;
 }
 public static synchronized Map<String,Object> last() { return last; }
 public static synchronized void clear() {
  if(last!=null) {
   consumed.add((String)last.get("identifier"));
   while(consumed.size()>128)consumed.remove(consumed.iterator().next());
   // The OS can restore the original launch Intent after process death. Keep
   // consumed identifiers on disk so that payload cannot reopen a conversation.
   if(preferences!=null)preferences.edit().putString("consumed",TextUtils.join("\n",consumed)).commit();
  }
  last=null;
 }
 public static void fromJSON(String raw) {
  if(raw==null||raw.length()>8192)return;
  try { JSONObject object=new JSONObject(raw);route(object.optString("event"),object.optString("grant_id"),object.optString("route_token")); } catch(Exception ignored) {}
 }
 public static void fromMap(Map<String,String> map) {
  if(map!=null) route(map.get("event"),map.get("grant_id"),map.get("route_token"));
 }
 public static void fromIntent(Intent intent) {
  if(intent==null)return;
  Bundle extras=intent.getExtras();if(extras==null)return;
  try {
   Object message=extras.get("key_message");
   // Xiaomi is optional: reference its serialized launch payload without a
   // compile-time dependency in builds that do not enable the Xiaomi SDK.
   if(message!=null&&"com.xiaomi.mipush.sdk.MiPushMessage".equals(message.getClass().getName())) {
    try { fromJSON((String)message.getClass().getMethod("getContent").invoke(message)); }
    catch(ReflectiveOperationException ignored) {}
   }
   route(extras.getString("event"),extras.getString("grant_id"),extras.getString("route_token"));
   for(String key:new String[]{"payload","data","params","action_parameters","selfDefineContentString"}) {
    Object value=extras.get(key);
    if(value instanceof String)fromJSON((String)value);
    else if(value instanceof Bundle){Bundle b=(Bundle)value;route(b.getString("event"),b.getString("grant_id"),b.getString("route_token"));}
   }
  }catch(RuntimeException ignored){}
 }
 private static void route(String event,String grant,String route) {
  if(!"message_created".equals(event)||grant==null||!grant.matches("[0-9a-fA-F-]{36}")||route==null||!route.matches("[A-Za-z0-9_-]{32,256}"))return;
  Map<String,String> data=new HashMap<>();data.put("event",event);data.put("grant_id",grant);data.put("route_token",route);
  Map<String,Object> response=new HashMap<>();response.put("data",data);response.put("date",System.currentTimeMillis());response.put("identifier",grant+":"+route);
  synchronized(NativePushResponse.class) { if(consumed.contains(response.get("identifier"))||(last!=null&&response.get("identifier").equals(last.get("identifier"))))return;last=response; }
  Listener current=listener;if(current!=null)current.response(response);
 }
 private NativePushResponse(){}
}
