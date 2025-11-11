import { Plus } from 'lucide-react'
import { useState } from 'react'
import JourneyForm from './new'
import { AppSidebar } from '@/components/nav/app-sidebar'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { SidebarProvider, SidebarInset, SidebarTrigger } from '@/components/ui/sidebar'

function Journeys() {
  const [isDialogOpen, setIsDialogOpen] = useState(false)

  const handleFormSubmitSuccess = () => {
    setIsDialogOpen(false)
  }

  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
          <SidebarTrigger />
          <div className="flex-1">
            <h1 className="text-xl font-semibold">Journeys</h1>
          </div>
          <Button onClick={() => setIsDialogOpen(true)}>
            <Plus />
            Create
          </Button>
        </header>

        <Dialog open={isDialogOpen} onOpenChange={setIsDialogOpen}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>New Journey</DialogTitle>
            </DialogHeader>
            <div className="py-4">
              <JourneyForm 
                onClose={() => setIsDialogOpen(false)} 
                onSubmitSuccess={handleFormSubmitSuccess} 
              />
            </div>
          </DialogContent>
        </Dialog>

        <main className="flex-1 p-4 sm:p-6 lg:p-8">
          <div className="max-w-4xl mx-auto">
            <h2 className="text-2xl font-bold mb-4">Your Journeys</h2>
            <p className="text-muted-foreground mb-6">Track and manage your journeys here.</p>
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
              <div className="border rounded-lg p-4 bg-card">
                <h3 className="font-semibold">Journey 1</h3>
                <p className="text-muted-foreground text-sm">Description of journey 1</p>
              </div>
              <div className="border rounded-lg p-4 bg-card">
                <h3 className="font-semibold">Journey 2</h3>
                <p className="text-muted-foreground text-sm">Description of journey 2</p>
              </div>
              <div className="border rounded-lg p-4 bg-card">
                <h3 className="font-semibold">Journey 3</h3>
                <p className="text-muted-foreground text-sm">Description of journey 3</p>
              </div>
            </div>
          </div>
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}

export default Journeys
