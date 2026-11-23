package cloud.baizhi.push;
import java.util.*;
import java.util.concurrent.ConcurrentHashMap;
public final class NativePushRegistry {
 private static final Map<String,String> tokens = new ConcurrentHashMap<>();
 private static final Set<String> pending = ConcurrentHashMap.newKeySet();
 private static final Set<String> unsupported = ConcurrentHashMap.newKeySet();
 public interface Listener { void changed(); }
 public static volatile Listener listener;
 public static void pending(String provider) { unsupported.remove(provider);pending.add(provider); }
 public static void complete(String provider) { pending.remove(provider); }
 public static void unsupported(String provider) {
  unsupported.add(provider);tokens.remove(provider);complete(provider);
 }
 public static List<String> unsupportedProviders() { return new ArrayList<>(new TreeSet<>(unsupported)); }
 public static boolean isPending() { return !pending.isEmpty(); }
 public static void token(String provider, String token) {
  if (token != null && !token.isEmpty() && token.length()<=8192) {
   unsupported.remove(provider);
   String old=tokens.put(provider,token);
   Listener current=listener;
   if (!token.equals(old) && current!=null) current.changed();
  }
  complete(provider);
 }
 public static List<Map<String,String>> snapshot() {
  List<Map<String,String>> result = new ArrayList<>();
  for (String provider:new TreeSet<>(tokens.keySet())) {
   Map<String,String> item=new HashMap<>();item.put("provider",provider);item.put("token",tokens.get(provider));result.add(item);
  }
  return result;
 }
 private NativePushRegistry() {}
}
