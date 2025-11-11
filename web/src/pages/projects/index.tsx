import { AppSidebar } from '@/components/nav/app-sidebar'
import { SidebarProvider, SidebarInset, SidebarTrigger } from '@/components/ui/sidebar'

function Projects() {
  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
          <SidebarTrigger />
          <div className="flex-1">
            <h1 className="text-xl font-semibold">Projects</h1>
          </div>
        </header>
        <main className="flex-1 p-4 sm:p-6 lg:p-8">
          <div className="max-w-4xl mx-auto">
            <h2 className="text-2xl font-bold mb-4">Your Projects</h2>
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
              <div className="border rounded-lg p-4 bg-card">
                <h3 className="font-semibold">Project 1</h3>
                <p className="text-muted-foreground text-sm">Description of project 1</p>
              </div>
              <div className="border rounded-lg p-4 bg-card">
                <h3 className="font-semibold">Project 2</h3>
                <p className="text-muted-foreground text-sm">Description of project 2</p>
              </div>
              <div className="border rounded-lg p-4 bg-card">
                <h3 className="font-semibold">Project 3</h3>
                <p className="text-muted-foreground text-sm">Description of project 3</p>
              </div>
            </div>
          </div>
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}

export default Projects