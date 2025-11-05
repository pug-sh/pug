import { SidebarProvider, SidebarInset, SidebarTrigger } from "@/components/ui/sidebar";
import { AppSidebar } from "@/components/nav/app-sidebar";

function Settings() {
  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
          <SidebarTrigger />
          <div className="flex-1">
            <h1 className="text-xl font-semibold">Settings</h1>
          </div>
        </header>
        <main className="flex-1 p-4 sm:p-6 lg:p-8">
          <div className="max-w-2xl mx-auto">
            <h2 className="text-2xl font-bold mb-4">Settings</h2>
            <div className="space-y-4">
              <div className="border rounded-lg p-4 bg-card">
                <h3 className="font-semibold">Account Settings</h3>
                <p className="text-muted-foreground text-sm">Manage your account information</p>
              </div>
              <div className="border rounded-lg p-4 bg-card">
                <h3 className="font-semibold">Display Settings</h3>
                <p className="text-muted-foreground text-sm">Customize the appearance</p>
              </div>
              <div className="border rounded-lg p-4 bg-card">
                <h3 className="font-semibold">Notification Settings</h3>
                <p className="text-muted-foreground text-sm">Configure your notifications</p>
              </div>
            </div>
          </div>
        </main>
      </SidebarInset>
    </SidebarProvider>
  );
}

export default Settings